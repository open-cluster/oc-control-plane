package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStoppingAnAlreadyClosedStreamSucceeds(t *testing.T) {
	t.Parallel()
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"error":"message_not_in_streaming_state"}`)
	}))
	defer vendor.Close()
	err := NewClient(vendor.URL).StopStream(context.Background(), "fixture", Stream{Channel: "C1", TS: "1700000100.1", Native: true})
	if err != nil {
		t.Fatalf("already closed stream was treated as a delivery failure: %v", err)
	}
}
