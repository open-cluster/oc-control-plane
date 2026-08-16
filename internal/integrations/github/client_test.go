package github

import (
	"context"
	"errors"
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

	listed, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 2)
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

func TestCommitsAreReadByStableIDInsideTheWindow(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/repositories/1296269/commits"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("since") != "2026-08-15T00:00:00Z" || query.Get("until") != "2026-08-16T00:00:00Z" {
			t.Errorf("window = [%s, %s]", query.Get("since"), query.Get("until"))
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Link", `<`+fake.URL+`/repositories/1296269/commits?page=2>; rel="next"`)
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
	fake.answers["/repositories/1296269/commits"] = func(writer http.ResponseWriter, _ *http.Request) {
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

func TestPullRequestsCarryStateAndBranches(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.answers["/repositories/1296269/pulls"] = func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("state") != "all" || query.Get("sort") != "updated" ||
			query.Get("direction") != "desc" {
			t.Errorf("query = %v; recency is what an investigation reads for", query)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`[
			{"number":98,"title":"Retry writes on conflict","state":"closed",
			 "merged_at":"2026-08-15T21:00:00Z","updated_at":"2026-08-15T21:00:00Z",
			 "user":{"login":"kai-dev"},"head":{"ref":"fix/retry"},"base":{"ref":"main"}}]`))
	}

	read, err := NewClient(fake.URL).PullRequests(testContext(t), "ghs_installation", PullsQuery{
		RepositoryID: 1296269, Limit: 10,
	})
	if err != nil {
		t.Fatalf("reading pull requests: %v", err)
	}
	pull := read.PullRequests[0]
	if pull.Number != 98 || pull.State != "closed" || pull.MergedAt == "" ||
		pull.Head != "fix/retry" || pull.Base != "main" {
		t.Errorf("pull = %+v", pull)
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
	_, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 10)
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
	_, err := NewClient(fake.URL).Repositories(context.Background(), "ghs_installation", 10)
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
	_, err := NewClient(fake.URL).Repositories(testContext(t), "ghs_installation", 10)
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
