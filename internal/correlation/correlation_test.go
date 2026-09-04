package correlation_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/correlation"
)

func TestMiddlewareReplacesInboundRequestIDAndSharesItThroughContext(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(correlation.Header, "caller-controlled")
	response := httptest.NewRecorder()
	var contextID string

	correlation.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		contextID = correlation.From(request.Context())
	})).ServeHTTP(response, request)

	headerID := response.Header().Get(correlation.Header)
	if headerID == "" || headerID == "caller-controlled" || headerID != contextID {
		t.Fatalf("header=%q context=%q", headerID, contextID)
	}
}
