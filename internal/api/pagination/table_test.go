package table_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/api/pagination"
)

// The contract is one shape for every list endpoint, so these tests are written against the
// contract rather than against any endpoint that speaks it. What they hold is the four
// properties a client can depend on without reading a handler: an unknown sort is refused, a
// limit above the ceiling is clamped, an absent total is null rather than zero, and a listing
// with nothing in it encodes as an empty array rather than as null.

var connections = table.Spec{
	Searchable:  true,
	Sortable:    []string{"name", "createdAt"},
	DefaultSort: table.Sort{Field: "createdAt", Descending: true},
	Filters:     []string{"environmentId", "integration"},
}

func TestAnAbsentQueryIsTheDefaultOrderAndSize(t *testing.T) {
	t.Parallel()

	query, err := table.Parse(url.Values{}, connections)
	if err != nil {
		t.Fatalf("parsing an empty query: %v", err)
	}
	if query.Sort != connections.DefaultSort {
		t.Errorf("sort = %+v, want the spec's default %+v", query.Sort, connections.DefaultSort)
	}
	if query.Limit != table.DefaultLimit {
		t.Errorf("limit = %d, want the default %d", query.Limit, table.DefaultLimit)
	}
	if query.Search != "" || query.Cursor != "" {
		t.Errorf("an empty query narrowed something: %+v", query)
	}
}

// A signed field name is the whole ordering vocabulary: "name" ascending, "-name" descending.
func TestSortIsASignedFieldName(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		asked string
		sort  table.Sort
	}{
		{"name", table.Sort{Field: "name"}},
		{"-name", table.Sort{Field: "name", Descending: true}},
		{"+name", table.Sort{Field: "name"}},
		{" -createdAt ", table.Sort{Field: "createdAt", Descending: true}},
	} {
		query, err := table.Parse(url.Values{"sort": {want.asked}}, connections)
		if err != nil {
			t.Fatalf("sort=%q: %v", want.asked, err)
		}
		if query.Sort != want.sort {
			t.Errorf("sort=%q parsed to %+v, want %+v", want.asked, query.Sort, want.sort)
		}
	}
}

// Refused rather than ignored. A sort silently dropped returns rows in an order the caller did
// not ask for and has no way to notice, which during an incident is a list that looks sorted.
func TestAnUnknownSortFieldIsRefused(t *testing.T) {
	t.Parallel()

	_, err := table.Parse(url.Values{"sort": {"-secretDigest"}}, connections)
	if !errors.Is(err, table.ErrUnknownSort) {
		t.Fatalf("sorting by an unoffered field = %v, want ErrUnknownSort", err)
	}
	if got := err.Error(); got == "" {
		t.Error("the refusal says nothing a caller could act on")
	}
}

// A filter nobody serves is refused for the same reason: narrowed by nothing looks exactly
// like narrowed by everything.
func TestAnUnknownFilterIsRefused(t *testing.T) {
	t.Parallel()

	_, err := table.Parse(url.Values{"organization": {"somebody-else"}}, connections)
	if !errors.Is(err, table.ErrUnknownFilter) {
		t.Fatalf("filtering by an unoffered field = %v, want ErrUnknownFilter", err)
	}
}

func TestSearchIsRefusedWhereItIsNotOffered(t *testing.T) {
	t.Parallel()

	unsearchable := table.Spec{Sortable: []string{"at"}, DefaultSort: table.Sort{Field: "at"}}
	if _, err := table.Parse(url.Values{"search": {"anything"}}, unsearchable); !errors.Is(
		err, table.ErrUnknownFilter) {
		t.Fatalf("searching a listing that does not offer it = %v, want ErrUnknownFilter", err)
	}
}

// Clamped, not refused. A caller asking for more than the ceiling wants as much as they can
// have, and answering 400 to that is a client that has to learn a number to work at all.
func TestALimitAboveTheCeilingIsClampedAndOneBelowTheFloorIsTheDefault(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		asked string
		limit int
	}{
		{"10", 10},
		{"100000", table.MaxLimit},
		{"0", table.DefaultLimit},
		{"-4", table.DefaultLimit},
		{"not a number", table.DefaultLimit},
		{"", table.DefaultLimit},
	} {
		query, err := table.Parse(url.Values{"limit": {want.asked}}, connections)
		if err != nil {
			t.Fatalf("limit=%q: %v", want.asked, err)
		}
		if query.Limit != want.limit {
			t.Errorf("limit=%q resolved to %d, want %d", want.asked, query.Limit, want.limit)
		}
	}
}

func TestFiltersAreReadableByName(t *testing.T) {
	t.Parallel()

	query, err := table.Parse(url.Values{
		"integration": {"kubernetes", "alertmanager"},
		"search":      {"  production  "},
		"cursor":      {"abc"},
	}, connections)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if got := query.Filter("integration"); got != "kubernetes" {
		t.Errorf("Filter(integration) = %q, want the first value", got)
	}
	if got := query.FilterAll("integration"); len(got) != 2 {
		t.Errorf("FilterAll(integration) = %v, want both values", got)
	}
	if query.Filter("environmentId") != "" {
		t.Error("an absent filter reports a value")
	}
	if query.Search != "production" {
		t.Errorf("search = %q, want it trimmed", query.Search)
	}
	if query.Cursor != "abc" {
		t.Errorf("cursor = %q", query.Cursor)
	}
}

// The envelope is the other half of the contract. total is NULLABLE because a cursor-paginated
// query over a large table cannot always answer it cheaply, and a fabricated count is worse
// than an absent one.
func TestTheEnvelopeEncodesAnAbsentTotalAsNullAndNoItemsAsAnEmptyArray(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(table.Answer[string](nil, "", nil))
	if err != nil {
		t.Fatalf("encoding an empty answer: %v", err)
	}
	const want = `{"items":[],"next":null,"total":null,"partial":[]}`
	if string(encoded) != want {
		t.Errorf("an empty answer encoded as %s, want %s", encoded, want)
	}
}

func TestTheEnvelopeCarriesTheCountItActuallyHas(t *testing.T) {
	t.Parallel()

	answer := table.Answer([]string{"a", "b"}, "next-page", table.Counted(2))
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	const want = `{"items":["a","b"],"next":"next-page","total":2,"partial":[]}`
	if string(encoded) != want {
		t.Errorf("encoded as %s, want %s", encoded, want)
	}
}

// A compiled listing pages like every other one. The alternative was an exemption — "this
// listing ignores cursor because it is short" — and a caller who paged it would get page one
// back forever with no way to tell that from having reached the end.
func TestCutWalksAnInMemoryListingExactlyOnce(t *testing.T) {
	t.Parallel()

	items := make([]int, 0, 13)
	for value := range 13 {
		items = append(items, value)
	}

	seen := make(map[int]int)
	query, err := table.Parse(url.Values{"limit": {"5"}}, connections)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	pages := 0
	for {
		page, next, cutErr := table.Cut(items, query)
		if cutErr != nil {
			t.Fatalf("cutting page %d: %v", pages, cutErr)
		}
		for _, value := range page {
			seen[value]++
		}
		pages++
		if next == "" {
			break
		}
		if pages > 10 {
			t.Fatal("the cursor never reached the end")
		}
		query.Cursor = next
	}

	if pages != 3 {
		t.Errorf("13 items at 5 a page took %d pages, want 3", pages)
	}
	for _, value := range items {
		if seen[value] != 1 {
			t.Errorf("%d was seen %d times in one walk", value, seen[value])
		}
	}
}

func TestCutRefusesATamperedCursor(t *testing.T) {
	t.Parallel()

	query, err := table.Parse(url.Values{"cursor": {"not-a-position"}}, connections)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if _, _, cutErr := table.Cut([]int{1, 2, 3}, query); !errors.Is(cutErr, table.ErrBadCursor) {
		t.Fatalf("a tampered cursor = %v, want ErrBadCursor: silently starting over shows an "+
			"operator the first page again and lets them believe they have seen the last", cutErr)
	}
}

// partial is how the backend says "I served this column with no data, and here is why", which
// is what lets a console render one honest notice instead of a column of "Not reported".
func TestPartialNamesTheFieldAndTheReason(t *testing.T) {
	t.Parallel()

	answer := table.Answer([]string{"a"}, "", nil,
		table.Partial{Field: "availableVersion", Reason: "no release channel is configured"})
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	const want = `{"items":["a"],"next":null,"total":null,` +
		`"partial":[{"field":"availableVersion","reason":"no release channel is configured"}]}`
	if string(encoded) != want {
		t.Errorf("encoded as %s, want %s", encoded, want)
	}
}
