// Package listing defines collection query and response conventions.
package listing

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	DefaultLimit    = 50
	MaxLimit        = 200
	MaxSearchLength = 256
	MaxCursorLength = 512
)

var (
	ErrUnknownSort   = errors.New("sort names a field this listing is not ordered by")
	ErrUnknownFilter = errors.New("the query names a parameter this listing does not accept")
	ErrInvalidLimit  = errors.New("limit must be an integer from 1 through 200")
	ErrInvalidQuery  = errors.New("query parameters must be scalar")
)

// Sort is a public field and direction.
type Sort struct {
	Field      string
	Descending bool
}

// Spec declares one collection's query capabilities.
type Spec struct {
	Searchable  bool
	Sortable    []string
	DefaultSort Sort
	Filters     []string
}

// Query is a parsed collection request.
type Query struct {
	Search  string
	Sort    Sort
	Cursor  string
	Limit   int
	filters map[string]string
}

func (q Query) Filter(name string) string {
	return q.filters[name]
}

// Parse rejects unsupported, repeated, blank, or malformed scalar parameters.
func Parse(values url.Values, spec Spec) (Query, error) {
	for name, given := range values {
		if len(given) != 1 {
			return Query{}, fmt.Errorf("%w: %q must appear once", ErrInvalidQuery, name)
		}
		if strings.TrimSpace(given[0]) == "" {
			return Query{}, fmt.Errorf("%w: %q must not be blank", ErrInvalidQuery, name)
		}
	}
	limit, err := limitFrom(values)
	if err != nil {
		return Query{}, err
	}
	query := Query{
		Search:  strings.TrimSpace(values.Get("search")),
		Sort:    spec.DefaultSort,
		Cursor:  values.Get("cursor"),
		Limit:   limit,
		filters: map[string]string{},
	}
	if len(query.Search) > MaxSearchLength {
		return Query{}, fmt.Errorf("%w: search is longer than %d characters", ErrInvalidQuery,
			MaxSearchLength)
	}
	if len(query.Cursor) > MaxCursorLength || query.Cursor != strings.TrimSpace(query.Cursor) {
		return Query{}, fmt.Errorf("%w: cursor is malformed", ErrInvalidQuery)
	}

	if asked, present := values["sort"]; present {
		if len(asked) != 1 {
			return Query{}, fmt.Errorf("%w: sort must appear once", ErrUnknownSort)
		}
		sort, err := sortFrom(asked[0], spec)
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
			query.filters[name] = given[0]
		default:
			return Query{}, fmt.Errorf("%w: %q; it accepts %v",
				ErrUnknownFilter, name, spec.Filters)
		}
	}
	return query, nil
}

// Refused reports a caller error.
func Refused(err error) bool {
	return errors.Is(err, ErrUnknownSort) || errors.Is(err, ErrUnknownFilter) ||
		errors.Is(err, ErrBadCursor) || errors.Is(err, ErrInvalidLimit) ||
		errors.Is(err, ErrInvalidQuery)
}

var reserved = map[string]bool{"search": true, "sort": true, "cursor": true, "limit": true}

func sortFrom(asked string, spec Spec) (Sort, error) {
	sort := Sort{Field: asked}
	if strings.HasPrefix(asked, "-") {
		sort = Sort{Field: asked[1:], Descending: true}
	}
	if !contains(spec.Sortable, sort.Field) {
		return Sort{}, fmt.Errorf("%w: %q; it is ordered by %v",
			ErrUnknownSort, sort.Field, spec.Sortable)
	}
	return sort, nil
}

func limitFrom(values url.Values) (int, error) {
	asked, present := values["limit"]
	if !present {
		return DefaultLimit, nil
	}
	if len(asked) != 1 {
		return 0, ErrInvalidLimit
	}
	size, err := strconv.Atoi(asked[0])
	if err != nil || size < 1 || size > MaxLimit {
		return 0, ErrInvalidLimit
	}
	return size, nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

type Partial struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type Envelope[T any] struct {
	Items   []T       `json:"items"`
	Next    *string   `json:"next"`
	Total   *int      `json:"total"`
	Partial []Partial `json:"partial"`
}

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

func Continuation(next string) *string {
	if next == "" {
		return nil
	}
	return &next
}

// Cut pages a stable in-memory collection with an ordinal cursor.
func Cut[T any](items []T, query Query) ([]T, string, error) {
	scope := query.Sort.Field
	if query.Sort.Descending {
		scope = "-" + scope
	}
	from, err := decodeOrdinal(query.Cursor, scope)
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
	return rest[:query.Limit], encodeOrdinal(scope, from+query.Limit), nil
}

var ErrBadCursor = errors.New("cursor is not a page position from a previous response")

func encodeOrdinal(scope string, at int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("#" + scope + ":" + strconv.Itoa(at)))
}

func decodeOrdinal(cursor, scope string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrBadCursor
	}
	digits, marked := strings.CutPrefix(string(raw), "#"+scope+":")
	if !marked {
		return 0, ErrBadCursor
	}
	at, err := strconv.Atoi(digits)
	if err != nil || at < 0 {
		return 0, ErrBadCursor
	}
	return at, nil
}
