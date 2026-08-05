package storage

import (
	"encoding/base64"
	"encoding/json"
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

// ordering is how one listing is sorted, and how a position in that order is rendered back into
// a cursor.
//
// The render function lives beside the column deliberately. It used to be a second switch on the
// same field names in the same file, and a fifth sortable field meant editing both — which is
// the shape of change where one edit lands and the other does not, and the symptom is a cursor
// that resumes from the wrong place rather than an error anybody sees.
type ordering struct {
	// column is the SQL expression rows are ordered by. It comes from a closed map keyed on a
	// field name the handler has already refused if unoffered, never from caller input.
	column string
	// cast is the type the cursor's text value is cast back to for the row-wise comparison, so
	// the resume compares the same way the ORDER BY sorted.
	cast string
	// render turns the last row of a page into the text half of its cursor.
	render func(Connection) string
}

// encodeSortCursor renders a page position for a listing ordered by a field the caller chose.
//
// It is a third codec beside the timestamp and ordinal ones because the ordering it resumes is
// genuinely different: the key may be a version string, a name or a fingerprint, and rendering
// those through a timestamp codec would either fail or, worse, round-trip to something that
// compares differently than it sorted. The value travels as text and the query casts it back to
// the type of the column it compares against, which is the same expression that produced the
// order.
func encodeSortCursor(value string, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte("~" + value + id.String()))
}

// decodeSortCursor reads such a position back. An empty cursor is the start of the listing,
// which is not an error; anything unreadable is, because silently starting over would show an
// operator the first page again and let them believe they had seen the last.
func decodeSortCursor(cursor string) (string, *uuid.UUID, error) {
	if cursor == "" {
		return "", nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", nil, ErrBadCursor
	}
	body, marked := strings.CutPrefix(string(raw), "~")
	if !marked {
		return "", nil, ErrBadCursor
	}
	// Split from the RIGHT rather than on a separator. A sort key is a customer's own text — a
	// Connection name, a cluster fingerprint — so any byte chosen to divide the two halves is a
	// byte a name could contain. The identifier is a fixed-width UUID, so the boundary is
	// arithmetic and there is nothing to escape.
	const identifierLength = 36
	if len(body) < identifierLength {
		return "", nil, ErrBadCursor
	}
	value := body[:len(body)-identifierLength]
	identifier := body[len(body)-identifierLength:]
	id, err := uuid.Parse(identifier)
	if err != nil {
		return "", nil, ErrBadCursor
	}
	return value, &id, nil
}

// decodeStringArray reads a JSONB array of strings into a slice, flattening null to empty. A
// caller asking what a relay serves should get a list they can range over rather than one they
// have to nil-check.
func decodeStringArray(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	if values == nil {
		return []string{}, nil
	}
	return values, nil
}
