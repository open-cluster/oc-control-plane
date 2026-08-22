// Package table holds the one query contract every list endpoint on the operator surface
// speaks, and the one envelope every one of them answers in.
//
// It exists because five listings that each invented their own paging, their own filter names
// and their own idea of what "no rows" looks like is five contracts a console has to hold and
// five places the same bug can be fixed four times. A caller who has learned one listing here
// has learned all of them.
//
// It is deliberately vocabulary and nothing else: no I/O, no database, no HTTP writing. What
// it owns is what a caller may ask for, which of those asks are refused rather than ignored,
// and what an answer looks like when part of it could not be served.
package table

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Bounds on a page.
//
// They mirror internal/storage's own clamp deliberately rather than importing it: this package
// is vocabulary and must not depend on persistence, and storage's clamp stays as the backstop
// for any read that never came from a request. The two numbers are the same number, and a
// change to one is a change to both.
const (
	// DefaultLimit is what a caller who named no size gets. It is the list, not the minimum: a
	// caller that named nothing wants the listing, and answering with one row would hide
	// exactly what the listing is read for.
	DefaultLimit = 50
	// MaxLimit is the ceiling. A caller above it is CLAMPED rather than refused, because
	// somebody asking for more than they can have wants as much as they can have, and a 400
	// there is a client that must learn a number before it works at all.
	MaxLimit = 200
)

// The refusals this contract produces. Both are things a listing could have ignored instead,
// and ignoring either produces a result the caller cannot tell from the one they asked for: an
// unknown sort returns rows in an order nobody chose, and an unknown filter returns everything
// while looking narrowed.
var (
	ErrUnknownSort   = errors.New("sort names a field this listing is not ordered by")
	ErrUnknownFilter = errors.New("the query names a parameter this listing does not accept")
)

// Sort is an ordering, parsed from a signed field name: `name` ascending, `-name` descending.
//
// It is a signed name rather than a field plus a direction parameter because the two can
// disagree — `sort=name&order=desc&order=asc` has no answer — and because one parameter
// survives a URL being pasted into an incident channel intact.
type Sort struct {
	Field      string
	Descending bool
}

// String renders a sort back into the form it was parsed from, so a response can echo what it
// applied rather than leaving a caller to assume it was honoured.
func (s Sort) String() string {
	if s.Descending {
		return "-" + s.Field
	}
	return s.Field
}

// Spec is what ONE listing accepts. It is declared next to the handler that serves it, so the
// answer to "what may I filter this by" is in the same file as the query that does it.
type Spec struct {
	// Searchable reports whether this listing accepts free text. A listing that does not offer
	// it refuses `search` rather than ignoring it.
	Searchable bool
	// Sortable is every field this listing may be ordered by. A field absent from it is
	// refused, which is what stops a caller ordering by a column that is not indexed — or one
	// that is not theirs to see.
	Sortable []string
	// DefaultSort is applied when the caller names none. It must be a Sortable field; a Spec
	// whose default is not offered is a programming error and Parse says so rather than
	// serving an order nobody can ask for again.
	DefaultSort Sort
	// Filters are the named narrowings this listing serves. The values arrive as text and the
	// endpoint interprets them, because what an `environmentId` is worth checking against is
	// the endpoint's business and not this package's.
	Filters []string
}

// Query is one caller's narrowed, ordered, paged request, already refused if it could not be
// served.
type Query struct {
	// Search is free text, trimmed. Empty means no narrowing.
	Search string
	Sort   Sort
	// Cursor resumes a previous page. It is opaque: a caller that took one apart would be
	// depending on the ordering rather than on the cursor, and the ordering is ours.
	Cursor string
	// Limit is already clamped to [1, MaxLimit].
	Limit   int
	filters url.Values
}

// Filter reads the first value of a named narrowing, or "" when the caller named none.
func (q Query) Filter(name string) string {
	values := q.filters[name]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// FilterAll reads every value of a named narrowing, for the filters that are genuinely a set —
// "state in (connected, degraded)" is one question, not two requests.
func (q Query) FilterAll(name string) []string { return q.filters[name] }

// Parse turns a request's query string into what a listing can serve, or into the reason it
// cannot.
//
// Every parameter is checked against the Spec. Anything the listing does not declare is
// refused, including the paging parameters' own misspellings — a caller who wrote `cursor` as
// `curser` and received page one again would have no way to tell that from the last page.
func Parse(values url.Values, spec Spec) (Query, error) {
	if !contains(spec.Sortable, spec.DefaultSort.Field) {
		return Query{}, fmt.Errorf(
			"table: this listing's default sort %q is not one of the fields it offers %v",
			spec.DefaultSort.Field, spec.Sortable)
	}

	query := Query{
		Search:  strings.TrimSpace(values.Get("search")),
		Sort:    spec.DefaultSort,
		Cursor:  strings.TrimSpace(values.Get("cursor")),
		Limit:   limitFrom(values.Get("limit")),
		filters: url.Values{},
	}

	if asked := strings.TrimSpace(values.Get("sort")); asked != "" {
		sort, err := sortFrom(asked, spec)
		if err != nil {
			return Query{}, err
		}
		query.Sort = sort
	}

	for name, given := range values {
		switch {
		case reserved[name]:
			if name == "search" && !spec.Searchable {
				return Query{}, fmt.Errorf("%w: %q", ErrUnknownFilter, name)
			}
		case contains(spec.Filters, name):
			query.filters[name] = given
		default:
			return Query{}, fmt.Errorf("%w: %q; it accepts %v",
				ErrUnknownFilter, name, spec.Filters)
		}
	}
	return query, nil
}

// Refused reports whether an error from Parse is the CALLER's mistake rather than the listing's.
//
// It exists because both are possible and they deserve opposite answers: an unoffered sort or
// filter is a 400 naming what went wrong, and a Spec whose own default sort it does not offer is
// a programming error that must be logged and answered as one. Each surface renders those in its
// own error shape, so the rendering stays local — but the classification is one rule, and two
// copies of it is one that eventually says 400 to a bug in our own table.
func Refused(err error) bool {
	return errors.Is(err, ErrUnknownSort) || errors.Is(err, ErrUnknownFilter) ||
		errors.Is(err, ErrBadCursor)
}

// reserved are the parameters every listing takes, whatever it is a listing of.
var reserved = map[string]bool{"search": true, "sort": true, "cursor": true, "limit": true}

// Reserved reports those names, sorted, so a caller can be TOLD the convention instead of
// discovering it one refusal at a time.
//
// It reads the same map Parse checks against. A restatement would be a second copy of the
// convention, and a listing accepting a parameter the published convention denies is the
// exact confusion this exists to end.
func Reserved() []string {
	names := make([]string, 0, len(reserved))
	for name := range reserved {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// SortPrefixes reports the signs a sort field may carry, in the order sortFrom tests them.
// Published for the same reason Reserved is: a caller who has to guess that `-name` means
// descending has to guess.
func SortPrefixes() []string { return []string{"-", "+"} }

func sortFrom(asked string, spec Spec) (Sort, error) {
	sort := Sort{Field: asked}
	switch {
	case strings.HasPrefix(asked, "-"):
		sort = Sort{Field: asked[1:], Descending: true}
	case strings.HasPrefix(asked, "+"):
		// A `+` in a query string decodes to a space, so it rarely survives the wire. It is
		// accepted anyway for the callers that encoded it properly, because refusing an
		// explicit ascending sort while accepting the implicit one would be a distinction
		// nobody could guess.
		sort = Sort{Field: asked[1:]}
	}
	sort.Field = strings.TrimSpace(sort.Field)
	if !contains(spec.Sortable, sort.Field) {
		return Sort{}, fmt.Errorf("%w: %q; it is ordered by %v",
			ErrUnknownSort, sort.Field, spec.Sortable)
	}
	return sort, nil
}

// limitFrom resolves how many rows to return. Nothing here is an error: an unreadable value is
// a caller who meant the default, and the ceiling is a bound rather than a rejection.
func limitFrom(asked string) int {
	size, err := strconv.Atoi(strings.TrimSpace(asked))
	if err != nil || size <= 0 {
		return DefaultLimit
	}
	return min(size, MaxLimit)
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// Partial is the backend saying "I served this field with no data, and here is why".
//
// It is the mechanism that lets a console render ONE honest notice instead of a column of
// "Not reported" — an absence stated once with its reason, rather than an absence repeated
// per row and left to be inferred.
type Partial struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Envelope is what every list endpoint answers with.
//
// Every field is present on the wire, including the empty ones. `items` is `[]` rather than
// `null` because a client should not have to handle two spellings of "nothing"; `next` and
// `total` are explicitly `null` because their absence is a fact — there is no next page, and
// this count is not one the query could answer cheaply — rather than a field somebody forgot.
type Envelope[T any] struct {
	Items []T `json:"items"`
	// Next resumes the next page and is null on the last one. Its presence is also how a
	// caller knows rows were left out: a truncated list that looks complete is how an operator
	// concludes a record is gone.
	Next *string `json:"next"`
	// Total is NULLABLE on purpose. A cursor-paginated query over a large table cannot always
	// answer it cheaply, and a fabricated count is worse than an absent one.
	Total   *int      `json:"total"`
	Partial []Partial `json:"partial"`
}

// Answer assembles an envelope, so no caller has to remember that an empty listing encodes as
// `[]` and an unknown total as `null`.
func Answer[T any](items []T, next string, total *int, partial ...Partial) Envelope[T] {
	if items == nil {
		items = []T{}
	}
	if partial == nil {
		partial = []Partial{}
	}
	envelope := Envelope[T]{Items: items, Total: total, Partial: partial}
	if next != "" {
		envelope.Next = &next
	}
	return envelope
}

// Counted states a total the query genuinely answered. It exists so a call site says
// `Counted(n)` rather than taking the address of a local nobody named.
func Counted(total int) *int { return &total }

// Cut applies a cursor and a limit to a listing that is already in its final order and already
// in memory, and reports the position to resume from.
//
// It exists so that a listing whose rows are COMPILED rather than queried still speaks the same
// contract as one that pages a table. The alternative was an exemption — "this listing ignores
// cursor and limit because it is short" — and an ignored parameter is exactly what the rest of
// this package refuses: a caller who paged a compiled listing would get page one back forever
// and have no way to tell that from having reached the end.
//
// The position is an ORDINAL rather than a key, because the ordering here is the ordering this
// process just produced and there is no stable key to resume from. That is sound for a compiled
// listing, which does not change between two requests, and it is why this is a separate helper
// rather than something offered to a listing over a table that rows are being inserted into.
func Cut[T any](items []T, query Query) ([]T, string, error) {
	from, err := decodeOrdinal(query.Cursor)
	if err != nil {
		return nil, "", err
	}
	if from >= len(items) {
		return []T{}, "", nil
	}
	rest := items[from:]
	if len(rest) <= query.Limit {
		return rest, "", nil
	}
	return rest[:query.Limit], encodeOrdinal(from + query.Limit), nil
}

// ErrBadCursor reports a resume point that did not come from a previous response. It is named
// here as well as in storage because both layers can produce one, and a caller matching on the
// storage value would miss the one an in-memory listing raises.
var ErrBadCursor = errors.New("cursor is not a page position from a previous response")

// encodeOrdinal renders an in-memory page position. It is opaque for the same reason every
// cursor here is: a caller that took it apart would be depending on the ordering rather than on
// the cursor, and the ordering is ours.
func encodeOrdinal(at int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("#" + strconv.Itoa(at)))
}

func decodeOrdinal(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrBadCursor
	}
	digits, marked := strings.CutPrefix(string(raw), "#")
	if !marked {
		return 0, ErrBadCursor
	}
	at, err := strconv.Atoi(digits)
	if err != nil || at < 0 {
		return 0, ErrBadCursor
	}
	return at, nil
}
