package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The client against a fake GitHub. The fake speaks the REST shapes, headers and refusal
// forms the real API does; the code under test is the real transport, decoding and bounds.

type fakeGitHub struct {
	*httptest.Server
	calls   map[string]*atomic.Int64
	answers map[string]func(writer http.ResponseWriter, request *http.Request)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	fake := &fakeGitHub{
		calls:   map[string]*atomic.Int64{},
		answers: map[string]func(http.ResponseWriter, *http.Request){},
	}
	fake.Server = httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			path := request.URL.Path
			counter, counted := fake.calls[path]
			if !counted {
				counter = &atomic.Int64{}
				fake.calls[path] = counter
			}
			counter.Add(1)

			answer, known := fake.answers[path]
			if !known {
				t.Errorf("the fake was asked for %q and has no answer for it", path)
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			answer(writer, request)
		}))
	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeGitHub) answer(path, body string) {
	f.answers[path] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	}
}

func (f *fakeGitHub) called(path string) int64 {
	counter, counted := f.calls[path]
	if !counted {
		return 0
	}
	return counter.Load()
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestInstallationIsReadUnderTheAppJWT(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/app/installations/77"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer the-app-jwt" {
			t.Errorf("installation read carried authorization %q; it is an APP operation", got)
		}
		if got := request.Header.Get("Accept"); !strings.Contains(got, "vnd.github") {
			t.Errorf("accept = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":77,
			"account":{"login":"acme-corp","type":"Organization"},
			"repository_selection":"selected","suspended_at":null}`))
	}

	installation, err := NewClient(fake.URL).Installation(testContext(t), "the-app-jwt", 77)
	if err != nil {
		t.Fatalf("reading the installation: %v", err)
	}
	if installation.Account != "acme-corp" || installation.AccountType != "Organization" {
		t.Errorf("installation = %+v", installation)
	}
	if installation.Suspended {
		t.Error("nothing suspended this installation")
	}
	if installation.RepositorySelection != "selected" {
		t.Errorf("selection = %q", installation.RepositorySelection)
	}
}

func TestASuspendedInstallationSaysSo(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answer("/app/installations/77", `{"id":77,
		"account":{"login":"acme-corp","type":"Organization"},
		"repository_selection":"all","suspended_at":"2026-08-01T00:00:00Z"}`)

	installation, err := NewClient(fake.URL).Installation(testContext(t), "the-app-jwt", 77)
	if err != nil {
		t.Fatalf("reading the installation: %v", err)
	}
	if !installation.Suspended {
		t.Error("a suspended installation must read as suspended; every call under it will fail")
	}
}

func TestAnUnknownInstallationIsATypedRefusal(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/app/installations/404404"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}

	_, err := NewClient(fake.URL).Installation(testContext(t), "the-app-jwt", 404404)
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("want a typed 404, got %v", err)
	}
}

func TestRepositoriesAreListedUnderTheInstallationToken(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer ghs_installation" {
			t.Errorf("repository listing carried %q; it is an INSTALLATION operation", got)
		}
		if got := request.URL.Query().Get("per_page"); got != "2" {
			t.Errorf("per_page = %q, want the caller's bound", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":5,"repositories":[
			{"id":1296269,"name":"payments","full_name":"acme-corp/payments",
			 "private":true,"archived":false,"default_branch":"main",
			 "description":"the payments service"},
			{"id":1296270,"name":"deploy","full_name":"acme-corp/deploy",
			 "private":true,"archived":false,"default_branch":"main","description":""}]}`))
	}

	listed, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 2, 1)
	if err != nil {
		t.Fatalf("listing repositories: %v", err)
	}
	if len(listed.Repositories) != 2 || listed.Repositories[0].ID != 1296269 {
		t.Errorf("repositories = %+v", listed.Repositories)
	}
	if listed.Repositories[0].FullName != "acme-corp/payments" {
		t.Errorf("full name = %q", listed.Repositories[0].FullName)
	}
	if !listed.Truncated {
		t.Error("five selected behind a bound of two must read as truncated")
	}
}

// grantsPayments teaches the fake the installation's repository grant, which is what
// the client resolves documented /repos/{owner}/{repo} paths from.
func grantsPayments(fake *fakeGitHub) {
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":1,"repositories":[
			{"id":1296269,"name":"payments","full_name":"acme-corp/payments"}]}`))
	}
}

func TestCommitsAreReadByStableIDInsideTheWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/commits"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("since") != "2026-08-15T00:00:00Z" || query.Get("until") != "2026-08-16T00:00:00Z" {
			t.Errorf("window = [%s, %s]", query.Get("since"), query.Get("until"))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Link", `<`+fake.URL+`/repos/acme-corp/payments/commits?page=2>; rel="next"`)
		_, _ = writer.Write([]byte(`[
			{"sha":"aaa111","commit":{"message":"raise the pool size",
			 "author":{"name":"Kai","date":"2026-08-15T20:00:00Z"}},
			 "author":{"login":"kai-dev"}}]`))
	}

	since := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	read, err := NewClient(fake.URL).Commits(testContext(t), "ghs_installation", CommitsQuery{
		RepositoryID: 1296269, Since: since, Until: since.Add(24 * time.Hour), Limit: 50,
	})
	if err != nil {
		t.Fatalf("reading commits: %v", err)
	}
	if len(read.Commits) != 1 || read.Commits[0].SHA != "aaa111" ||
		read.Commits[0].Author != "kai-dev" {
		t.Errorf("commits = %+v", read.Commits)
	}
	if !read.Truncated {
		t.Error("a further page means the window holds more, and that must be flagged")
	}
}

func TestAnEmptyRepositoryAnswersNoCommitsRatherThanAnError(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/commits"] = func(writer http.ResponseWriter, _ *http.Request) {
		// GitHub's own answer for a repository with no commits yet.
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"message":"Git Repository is empty."}`))
	}

	read, err := NewClient(fake.URL).Commits(testContext(t), "ghs_installation", CommitsQuery{
		RepositoryID: 1296269, Limit: 10,
	})
	if err != nil {
		t.Fatalf("an empty repository is an answer, not an error: %v", err)
	}
	if len(read.Commits) != 0 || read.Truncated {
		t.Errorf("read = %+v", read)
	}
}

func TestPullRequestCarriesItsIntentAndBranches(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/pulls/98", `
		{"number":98,"title":"Retry writes on conflict","state":"closed",
		 "body":"Conflict storms were exhausting the pool; retry with backoff.",
		 "merged":true,"merged_at":"2026-08-15T21:00:00Z","updated_at":"2026-08-15T21:00:00Z",
		 "user":{"login":"kai-dev"},"head":{"ref":"fix/retry","sha":"abc123"},
		 "base":{"ref":"main"},"html_url":"https://github.com/acme-corp/payments/pull/98"}`)

	pull, err := NewClient(fake.URL).PullRequest(testContext(t), "ghs_installation", 1296269, 98)
	if err != nil {
		t.Fatalf("reading the pull request: %v", err)
	}
	if pull.Number != 98 || !pull.Merged || pull.Head != "fix/retry" ||
		pull.HeadSHA != "abc123" || !strings.Contains(pull.Body, "backoff") ||
		pull.HTMLURL == "" {
		t.Errorf("pull = %+v; the description IS the intent", pull)
	}
}

func TestPullRequestFilesFlagTheRemainder(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/pulls/98/files"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Link", `<next>; rel="next"`)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[
			{"filename":"config/pool.yaml","status":"modified","additions":1,"deletions":1,
			 "patch":"@@ -1 +1 @@\n-max: 10\n+max: 100"}]`))
	}

	files, truncated, err := NewClient(fake.URL).PullRequestFiles(
		testContext(t), "ghs_installation", 1296269, 98, 100)
	if err != nil {
		t.Fatalf("reading the files: %v", err)
	}
	if len(files) != 1 || files[0].Path != "config/pool.yaml" ||
		!strings.Contains(files[0].Patch, "max: 100") {
		t.Errorf("files = %+v", files)
	}
	if !truncated {
		t.Error("a next page means the listing holds more; that must be flagged")
	}
}

func TestCheckRunsReadAgainstTheHeadSHA(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/commits/abc123/check-runs", `
		{"total_count":2,"check_runs":[
			{"name":"unit tests","status":"completed","conclusion":"failure"},
			{"name":"lint","status":"completed","conclusion":"success"}]}`)

	checks, truncated, err := NewClient(fake.URL).CheckRuns(
		testContext(t), "ghs_installation", 1296269, "abc123", 50)
	if err != nil {
		t.Fatalf("reading check runs: %v", err)
	}
	if len(checks) != 2 || checks[0].Conclusion != "failure" || truncated {
		t.Errorf("checks = %+v truncated=%v", checks, truncated)
	}
}

func TestCommitDetailCarriesFilesAndPatches(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/commits/abc123", `
		{"sha":"abc123","commit":{"message":"raise pool ceiling",
		 "author":{"name":"Kai","date":"2026-08-15T20:00:00Z"}},
		 "author":{"login":"kai-dev"},
		 "html_url":"https://github.com/acme-corp/payments/commit/abc123",
		 "files":[{"filename":"config/pool.yaml","status":"modified",
		           "additions":1,"deletions":1,"patch":"@@ -1 +1 @@\n-max: 10\n+max: 100"}]}`)

	detail, err := NewClient(fake.URL).Commit(testContext(t), "ghs_installation", 1296269, "abc123")
	if err != nil {
		t.Fatalf("reading the commit: %v", err)
	}
	if detail.Author != "kai-dev" || len(detail.Files) != 1 ||
		!strings.Contains(detail.Files[0].Patch, "max: 100") || detail.HTMLURL == "" {
		t.Errorf("detail = %+v; the diff is the change itself", detail)
	}
}

func TestAnOversizedDiffIsRefusedWithTheReason(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/commits/huge"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"message":"Sorry, this diff is taking too long to generate."}`))
	}

	_, err := NewClient(fake.URL).Commit(testContext(t), "ghs_installation", 1296269, "huge")
	if err == nil || !strings.Contains(err.Error(), "diff") {
		t.Fatalf("the vendor's oversized-diff refusal must be named, got %v", err)
	}
}

func TestWorkflowRunsCarryTheCreatedRange(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/actions/runs"] = func(writer http.ResponseWriter, request *http.Request) {
		created := request.URL.Query().Get("created")
		if !strings.Contains(created, "2026-08-15T20:00:00Z..2026-08-15T22:00:00Z") {
			t.Errorf("created = %q; the window travels as the vendor's own filter", created)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":1,"workflow_runs":[
			{"id":42,"name":"deploy","head_branch":"main","head_sha":"abc123",
			 "event":"push","status":"completed","conclusion":"failure",
			 "created_at":"2026-08-15T21:00:00Z",
			 "html_url":"https://github.com/acme-corp/payments/actions/runs/42"}]}`))
	}

	runs, err := NewClient(fake.URL).WorkflowRuns(testContext(t), "ghs_installation", RunsQuery{
		RepositoryID: 1296269,
		Since:        time.Date(2026, 8, 15, 20, 0, 0, 0, time.UTC),
		Until:        time.Date(2026, 8, 15, 22, 0, 0, 0, time.UTC),
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("reading runs: %v", err)
	}
	if len(runs.Runs) != 1 || runs.Runs[0].Conclusion != "failure" || runs.Runs[0].ID != 42 {
		t.Errorf("runs = %+v", runs.Runs)
	}
}

func TestRunJobsNameTheFailedStep(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/actions/runs/42/jobs", `
		{"jobs":[{"id":9,"name":"test","status":"completed","conclusion":"failure",
		          "steps":[{"name":"checkout","conclusion":"success"},
		                   {"name":"go test","conclusion":"failure"}]}]}`)

	jobs, err := NewClient(fake.URL).RunJobs(testContext(t), "ghs_installation", 1296269, 42, 50)
	if err != nil {
		t.Fatalf("reading jobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].FailedStep != "go test" {
		t.Errorf("jobs = %+v; the failing step is the lead", jobs)
	}
}

// The log endpoint 302s to an expiring storage URL; the tail is asked for with a Range
// request, and Go's client drops Authorization on the cross-host redirect by itself.
func TestJobLogFollowsTheRedirectAndReadsTheTail(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fullLog := strings.Repeat("line of build output\n", 100)
	fake.answers["/repos/acme-corp/payments/actions/jobs/9/logs"] = func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, fake.URL+"/logstore", http.StatusFound)
	}
	fake.answers["/logstore"] = func(writer http.ResponseWriter, request *http.Request) {
		rangeHeader := request.Header.Get("Range")
		if !strings.HasPrefix(rangeHeader, "bytes=-") {
			t.Errorf("range = %q; the tail is asked for, not the whole log", rangeHeader)
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			len(fullLog)-256, len(fullLog)-1, len(fullLog)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte(fullLog[len(fullLog)-256:]))
	}

	log, truncated, err := NewClient(fake.URL).JobLog(
		testContext(t), "ghs_installation", 1296269, 9, 16<<10)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if !truncated || !strings.HasSuffix(log, "line of build output\n") {
		t.Errorf("log tail = %q… truncated=%v", log[:20], truncated)
	}
}

// A suffix range wider than the log answers 206 carrying the log whole — which is the
// whole answer, not a truncation, and must not be flagged as one.
func TestJobLogDoesNotClaimTruncationWhenThePartialAnswerIsWhole(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	wholeLog := "short build output\nall green\n"
	fake.answers["/repos/acme-corp/payments/actions/jobs/9/logs"] = func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, fake.URL+"/logstore", http.StatusFound)
	}
	fake.answers["/logstore"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d",
			len(wholeLog)-1, len(wholeLog)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write([]byte(wholeLog))
	}

	log, truncated, err := NewClient(fake.URL).JobLog(
		testContext(t), "ghs_installation", 1296269, 9, 16<<10)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if truncated || log != wholeLog {
		t.Errorf("log = %q truncated=%v; a whole answer must not claim truncation", log, truncated)
	}
}

// A storage that ignores Range answers 200 with everything; the tail is cut locally.
func TestJobLogTailsLocallyWhenRangeIsIgnored(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/actions/jobs/9/logs"] = func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, fake.URL+"/logstore", http.StatusFound)
	}
	fake.answers["/logstore"] = func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(strings.Repeat("x", 100) + "THE END"))
	}

	log, truncated, err := NewClient(fake.URL).JobLog(
		testContext(t), "ghs_installation", 1296269, 9, 32)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	if len(log) != 32 || !strings.HasSuffix(log, "THE END") || !truncated {
		t.Errorf("log = %q truncated=%v; the tail is the answer", log, truncated)
	}
}

func TestFileReadsRawAndFlagsTheBound(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answers["/repos/acme-corp/payments/contents/config/pool.yaml"] = func(writer http.ResponseWriter, request *http.Request) {
		if accept := request.Header.Get("Accept"); !strings.Contains(accept, "raw") {
			t.Errorf("accept = %q; contents over 1 MB are raw-only, so raw is the one path", accept)
		}
		if ref := request.URL.Query().Get("ref"); ref != "abc123" {
			t.Errorf("ref = %q", ref)
		}
		_, _ = writer.Write([]byte("max: 100\nburst: 10\n"))
	}

	content, err := NewClient(fake.URL).File(
		testContext(t), "ghs_installation", 1296269, "config/pool.yaml", "abc123", 10)
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if content.Content != "max: 100\nb" || !content.Truncated {
		t.Errorf("content = %+v; the bound cuts with the flag set", content)
	}
}

func TestReleasesListWhatShipped(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	grantsPayments(fake)
	fake.answer("/repos/acme-corp/payments/releases", `[
		{"name":"v2.3.0","tag_name":"v2.3.0","published_at":"2026-08-15T19:00:00Z",
		 "prerelease":false,"author":{"login":"kai-dev"},
		 "html_url":"https://github.com/acme-corp/payments/releases/tag/v2.3.0"}]`)

	read, err := NewClient(fake.URL).Releases(testContext(t), "ghs_installation", 1296269, 10)
	if err != nil {
		t.Fatalf("reading releases: %v", err)
	}
	if len(read.Releases) != 1 || read.Releases[0].Tag != "v2.3.0" ||
		read.Releases[0].PublishedAt == "" {
		t.Errorf("releases = %+v", read.Releases)
	}
}

func TestARenamedRepositoryIsReResolvedOnce(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	renamed := &atomic.Bool{}
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		name := "acme-corp/payments"
		if renamed.Load() {
			name = "acme-corp/payments-service"
		}
		_, _ = writer.Write([]byte(`{"total_count":1,"repositories":[
			{"id":1296269,"name":"payments","full_name":"` + name + `"}]}`))
	}
	fake.answers["/repos/acme-corp/payments/releases"] = func(writer http.ResponseWriter, _ *http.Request) {
		// The rename happened between resolution and this read.
		renamed.Store(true)
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write([]byte(`{"message":"Not Found"}`))
	}
	fake.answers["/repos/acme-corp/payments-service/releases"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	if _, err := NewClient(fake.URL).Releases(testContext(t), "ghs_installation",
		1296269, 10); err != nil {
		t.Fatalf("a rename must re-resolve and retry once, got %v", err)
	}
	if fake.called("/installation/repositories") != 2 {
		t.Errorf("resolution ran %d times, want 2 (once cached, once refreshed)",
			fake.called("/installation/repositories"))
	}
}

func TestNameResolutionWalksTheGrantPages(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "1" {
			writer.Header().Set("Link", `<`+fake.URL+`/installation/repositories?page=2>; rel="next"`)
			_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
				{"id":1,"name":"first","full_name":"acme-corp/first"}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"total_count":2,"repositories":[
			{"id":1296269,"name":"payments","full_name":"acme-corp/payments"}]}`))
	}
	fake.answers["/repos/acme-corp/payments/releases"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[]`))
	}

	if _, err := NewClient(fake.URL).Releases(testContext(t), "ghs_installation",
		1296269, 10); err != nil {
		t.Fatalf("a repository on the grant's second page must be resolvable: %v", err)
	}
}

func TestA410IsNamedAsTheRetiredAPIVersion(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusGone)
		_, _ = writer.Write([]byte(`{"message":"Gone"}`))
	}

	_, err := NewClient(fake.URL).Releases(testContext(t), "ghs_installation",
		1296269, 10)
	if !errors.Is(err, ErrAPIVersionRetired) {
		t.Fatalf("a 410 must self-diagnose as the retired API version, got %v", err)
	}
}

func TestRateLimitingHonoursRetryAfterOnceThenRefuses(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		if fake.called("/installation/repositories") == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"total_count":0,"repositories":[]}`))
	}

	started := time.Now()
	_, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 10, 1)
	if err != nil {
		t.Fatalf("a rate-limited read must succeed on the retry: %v", err)
	}
	if fake.called("/installation/repositories") != 2 || time.Since(started) < time.Second {
		t.Error("the retry must happen exactly once, after the wait the vendor asked for")
	}
}

func TestAWaitPastTheCapIsRefusedEvenWithoutADeadline(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "86400")
		writer.WriteHeader(http.StatusTooManyRequests)
	}

	started := time.Now()
	_, err := NewClient(fake.URL).Repositories(context.Background(), "ghs_installation", 10, 1)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Error("a caller with no deadline was parked on the vendor's say-so")
	}
	if fake.called("/installation/repositories") != 1 {
		t.Errorf("called %d times; a day-long wait is not worth taking",
			fake.called("/installation/repositories"))
	}
}

func TestAnExhaustedRateBudgetIsTypedWithoutSleeping(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/installation/repositories"] = func(writer http.ResponseWriter, _ *http.Request) {
		// The primary rate limit: 403 with a reset far away and no Retry-After.
		writer.Header().Set("X-RateLimit-Remaining", "0")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}

	started := time.Now()
	_, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 10, 1)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Error("an exhausted hourly budget is not worth sleeping towards")
	}
}

func TestTheDefaultBaseURLIsTheVendors(t *testing.T) {
	t.Parallel()

	if NewClient("").baseURL != "https://api.github.com" {
		t.Errorf("default base URL = %q", NewClient("").baseURL)
	}
}
