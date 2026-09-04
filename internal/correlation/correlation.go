// Package correlation assigns server-controlled request identifiers.
package correlation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

const Header = "X-Request-Id"

type contextKey struct{}

func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		id := newID()
		writer.Header().Set(Header, id)
		ctx := context.WithValue(request.Context(), contextKey{}, id)
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func From(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "uncorrelated"
	}
	return hex.EncodeToString(raw[:])
}
