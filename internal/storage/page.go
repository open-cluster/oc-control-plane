package storage

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Page is which slice of a listing to return. It is not relay-specific: every paged read on
// this surface takes the same shape, and naming it for the first caller would have made
// "Relay" mean something it does not the moment the second one arrived.
type Page struct {
	// Limit is how many to return. Zero means the default rather than none: a caller that names
	// no size wants the list, and answering with one row would hide the very findings this is
	// read for.
	Limit int
	// After resumes from a previous page's Next. An empty value starts at the beginning.
	After string
}

// Bounds on a page. An operator asking for everything still gets a bounded answer,
// because an unbounded list is a query whose cost belongs to whoever calls it most — and the
// cursor is what makes that bound a page rather than a ceiling on what can ever be seen.
const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// ErrBadCursor reports a resume point that did not come from a previous page.
var ErrBadCursor = errors.New("cursor is not a page position")

// pageLimit resolves how many rows to return. A caller that named nothing gets the default,
// not the minimum.
func pageLimit(asked int) int {
	if asked <= 0 {
		return defaultPageSize
	}
	return min(asked, maxPageSize)
}

// encodeCursor renders a page position. It is opaque on purpose: a caller that took it apart
// would be depending on the ordering rather than on the cursor, and the ordering is ours.
func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(strconv.FormatInt(at.UnixNano(), 10) + ":" + id.String()))
}

// encodeOrdinalCursor renders a page position in a listing ordered by a stable per-case ordinal.
// It is a second codec rather than a reuse of the timestamp one because the ordering it resumes is
// genuinely different: an ordinal is assigned once and never moves, which is what lets a section
// stay in one order while the case it belongs to is still growing.
func encodeOrdinalCursor(ordinal int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("#" + strconv.Itoa(ordinal)))
}

// decodeOrdinalCursor reads such a position back. Zero is the start of the listing.
func decodeOrdinalCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrBadCursor
	}
	digits, found := strings.CutPrefix(string(raw), "#")
	if !found {
		return 0, ErrBadCursor
	}
	ordinal, err := strconv.Atoi(digits)
	if err != nil || ordinal < 0 {
		return 0, ErrBadCursor
	}
	return ordinal, nil
}

// decodeCursor reads a page position back. An empty cursor is the start of the list, which is
// not an error; anything unreadable is, because silently starting over would show an operator
// the first page again and let them believe they had seen the last.
func decodeCursor(cursor string) (*time.Time, *uuid.UUID, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	nanos, identifier, found := strings.Cut(string(raw), ":")
	if !found {
		return nil, nil, ErrBadCursor
	}
	unixNano, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	id, err := uuid.Parse(identifier)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	at := time.Unix(0, unixNano)
	return &at, &id, nil
}
