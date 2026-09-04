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

type Page struct {
	Limit int
	After string
}

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

var ErrBadCursor = errors.New("cursor is not a page position")

func pageLimit(asked int) int {
	if asked <= 0 {
		return defaultPageSize
	}
	return min(asked, maxPageSize)
}

func encodeCursor(scope string, at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte("@" + scope + ":" + strconv.FormatInt(at.UnixNano(), 10) + ":" + id.String()))
}

func decodeCursor(cursor, scope string) (*time.Time, *uuid.UUID, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	body, marked := strings.CutPrefix(string(raw), "@"+scope+":")
	if !marked {
		return nil, nil, ErrBadCursor
	}
	nanos, identifier, found := strings.Cut(body, ":")
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

func sortScope(field string, descending bool) string {
	if descending {
		return "-" + field
	}
	return field
}

func encodeSortCursor(scope, value string, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte("~" + scope + ":" + value + id.String()))
}

func decodeSortCursor(cursor, scope string) (string, *uuid.UUID, error) {
	if cursor == "" {
		return "", nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", nil, ErrBadCursor
	}
	body, marked := strings.CutPrefix(string(raw), "~"+scope+":")
	if !marked {
		return "", nil, ErrBadCursor
	}
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

func encodeTimeSortCursor(scope string, at time.Time, id uuid.UUID) string {
	return encodeSortCursor(scope, at.Format(time.RFC3339Nano), id)
}

func decodeTimeSortCursor(cursor, scope string) (*time.Time, *uuid.UUID, error) {
	value, id, err := decodeSortCursor(cursor, scope)
	if err != nil || id == nil {
		return nil, id, err
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, nil, ErrBadCursor
	}
	return &at, id, nil
}

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
