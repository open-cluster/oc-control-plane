package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
)

const (
	pollInterval = 500 * time.Millisecond
	heartbeat    = 15 * time.Second
	writeTimeout = 10 * time.Second
	readTimeout  = 15 * time.Second
	streamLimit  = 31 * time.Minute
	authInterval = 30 * time.Second
	authTimeout  = 5 * time.Second
)

// Stream writes changed snapshots. Its request context never owns the execution.
func Stream[T interface{ Terminal() bool }](writer http.ResponseWriter, request *http.Request, shutdown context.Context,
	resolve func(*http.Request) (authz.Principal, error), permission authz.Permission,
	read func(context.Context) (T, error), fail func(http.ResponseWriter, *http.Request, error),
) {
	if resolve == nil {
		http.Error(writer, "stream unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), streamLimit)
	defer cancel()
	if shutdown != nil {
		stop := context.AfterFunc(shutdown, cancel)
		defer stop()
		if shutdown.Err() != nil {
			return
		}
	}
	request = request.WithContext(ctx)
	original := authz.MustPrincipal(ctx)
	nextAuth := time.Now().Add(authInterval)
	stillAuthorized := func() bool {
		if ctx.Err() != nil {
			return false
		}
		now := time.Now()
		if now.Before(nextAuth) {
			return true
		}
		check, stop := context.WithTimeout(ctx, authTimeout)
		defer stop()
		current, err := resolve(request.WithContext(check))
		nextAuth = now.Add(authInterval)
		return err == nil && check.Err() == nil && current.UserID() == original.UserID() &&
			current.SessionID() == original.SessionID() && current.Organization() == original.Organization() &&
			current.HavePermission(permission)
	}
	controller := http.NewResponseController(writer)
	var last []byte
	var lastWrite time.Time
	defer func() {
		if last != nil {
			// net/http still needs a deadline for the closing chunk after an idle stream.
			_ = controller.SetWriteDeadline(time.Now().Add(writeTimeout))
		}
	}()
	for stillAuthorized() {
		readCtx, stop := context.WithTimeout(ctx, readTimeout)
		snapshot, err := read(readCtx)
		stop()
		if err != nil {
			if last == nil && ctx.Err() == nil {
				fail(writer, request, err)
			}
			return
		}
		body, err := json.Marshal(snapshot)
		if err != nil || !stillAuthorized() {
			return
		}
		if !bytes.Equal(last, body) || time.Since(lastWrite) >= heartbeat {
			if err := controller.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
				if last == nil {
					http.Error(writer, "stream unavailable", http.StatusInternalServerError)
				}
				return
			}
			if last == nil {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("Cache-Control", "no-cache")
				writer.Header().Set("X-Accel-Buffering", "no")
			}
			if !bytes.Equal(last, body) {
				_, err = fmt.Fprintf(writer, "event: snapshot\ndata: %s\n\n", body)
				last = body
			} else {
				_, err = fmt.Fprint(writer, ": keep-alive\n\n")
			}
			if err != nil || controller.Flush() != nil || snapshot.Terminal() {
				return
			}
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}
