package listing_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	table "github.com/open-cluster/oc-control-plane/internal/api/listing"
)

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

func TestSortIsASignedFieldName(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		asked string
		sort  table.Sort
	}{
		{"name", table.Sort{Field: "name"}},
		{"-name", table.Sort{Field: "name", Descending: true}},
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

func TestMalformedSortsAreRefused(t *testing.T) {
	t.Parallel()

	for _, asked := range []string{"+name", " name", "name ", "-", "--name", ""} {
		if _, err := table.Parse(url.Values{"sort": {asked}}, connections); err == nil {
			t.Errorf("sort=%q was accepted", asked)
		}
	}
}

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

func TestDefaultSortNeedNotBeClientSelectable(t *testing.T) {
	t.Parallel()

	fixed := table.Spec{DefaultSort: table.Sort{Field: "createdAt", Descending: true}}
	query, err := table.Parse(url.Values{}, fixed)
	if err != nil {
		t.Fatalf("parsing fixed ordering: %v", err)
	}
	if query.Sort != fixed.DefaultSort {
		t.Fatalf("sort = %+v, want %+v", query.Sort, fixed.DefaultSort)
	}
	if _, err = table.Parse(url.Values{"sort": {"createdAt"}}, fixed); !errors.Is(err, table.ErrUnknownSort) {
		t.Fatalf("client-selected fixed sort = %v, want ErrUnknownSort", err)
	}
}

func TestMalformedLimitsAreRefused(t *testing.T) {
	t.Parallel()

	for _, asked := range []string{"100000", "0", "-4", "not a number", ""} {
		if _, err := table.Parse(url.Values{"limit": {asked}}, connections); err == nil {
			t.Errorf("limit=%q was accepted", asked)
		}
	}

	query, err := table.Parse(url.Values{"limit": {"10"}}, connections)
	if err != nil || query.Limit != 10 {
		t.Fatalf("limit=10 resolved to %d, %v", query.Limit, err)
	}
}

func TestFiltersAreReadableByName(t *testing.T) {
	t.Parallel()

	query, err := table.Parse(url.Values{
		"integration": {"kubernetes"},
		"search":      {"  production  "},
		"cursor":      {"abc"},
	}, connections)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	if got := query.Filter("integration"); got != "kubernetes" {
		t.Errorf("Filter(integration) = %q, want the first value", got)
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

func TestDuplicateScalarParametersAreRefused(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"search", "sort", "limit", "cursor", "integration"} {
		values := url.Values{name: {"name", "createdAt"}}
		if _, err := table.Parse(values, connections); err == nil {
			t.Errorf("duplicate %s was accepted", name)
		}
	}
}

func TestBlankScalarParametersAreRefused(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"search", "sort", "limit", "cursor", "integration"} {
		if _, err := table.Parse(url.Values{name: {"  "}}, connections); err == nil {
			t.Errorf("blank %s was accepted", name)
		}
	}
}

func TestSearchAndCursorAreBounded(t *testing.T) {
	t.Parallel()

	if _, err := table.Parse(url.Values{"search": {string(make([]byte, 257))}}, connections); err == nil {
		t.Error("oversized search was accepted")
	}
	if _, err := table.Parse(url.Values{"cursor": {string(make([]byte, 513))}}, connections); err == nil {
		t.Error("oversized cursor was accepted")
	}
}

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

	total := 2
	answer := table.Answer([]string{"a", "b"}, "next-page", &total)
	encoded, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	const want = `{"items":["a","b"],"next":"next-page","total":2,"partial":[]}`
	if string(encoded) != want {
		t.Errorf("encoded as %s, want %s", encoded, want)
	}
}

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

func TestCutCursorIsBoundToItsOrdering(t *testing.T) {
	t.Parallel()

	query, err := table.Parse(url.Values{"limit": {"1"}}, connections)
	if err != nil {
		t.Fatal(err)
	}
	_, next, err := table.Cut([]int{1, 2}, query)
	if err != nil || next == "" {
		t.Fatalf("first page next=%q err=%v", next, err)
	}
	query.Cursor = next
	query.Sort = table.Sort{Field: "name"}
	if _, _, err = table.Cut([]int{1, 2}, query); !errors.Is(err, table.ErrBadCursor) {
		t.Fatalf("cursor reused with another order = %v, want ErrBadCursor", err)
	}
}

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
