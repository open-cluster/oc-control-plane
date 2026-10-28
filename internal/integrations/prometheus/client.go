package prometheus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	prom "github.com/prometheus/client_golang/api/prometheus/v1"
)

const (
	requestTimeout   = 10 * time.Second
	queryTimeout     = 5 * time.Second
	maxResponseBytes = 2 << 20
)

var (
	errDenied   = errors.New("prometheus authentication or query permission denied")
	errRedirect = errors.New("prometheus redirects are not allowed; configure the final URL")
	errResponse = errors.New("prometheus response exceeds the safe size limit; narrow the query")
)

func newClient(address, credential string) (prom.API, error) {
	addressUrl, err := url.Parse(address)
	if err != nil || len(address) > 2048 ||
		(addressUrl.Scheme != "http" && addressUrl.Scheme != "https") || addressUrl.Hostname() == "" ||
		addressUrl.User != nil || addressUrl.RawQuery != "" || addressUrl.ForceQuery || addressUrl.Fragment != "" ||
		addressUrl.RawPath != "" || addressUrl.Opaque != "" {
		return nil, errors.New("prometheus URL must be an absolute HTTP(S) URL without credentials, query, fragment or escaped path")
	}
	for _, segment := range strings.Split(addressUrl.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("prometheus URL must not contain path traversal")
		}
	}
	if strings.ContainsAny(credential, "\r\n") {
		return nil, errors.New("prometheus bearer credential contains an invalid line break")
	}
	addressUrl.Path = strings.TrimRight(addressUrl.Path, "/")
	client, err := api.NewClient(api.Config{
		Address: addressUrl.String(),
		Client: &http.Client{
			Timeout: requestTimeout,
			Transport: &readTransport{
				endpoint:   addressUrl,
				credential: credential,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
		},
	})
	if err != nil {
		return nil, errors.New("prometheus client could not be configured")
	}
	return prom.NewAPI(client), nil
}

// The SDK reads response bodies in full; enforce a decoded-byte limit before it does so.
type readTransport struct {
	endpoint   *url.URL
	credential string
}

func (t *readTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.endpoint.Scheme || request.URL.Host != t.endpoint.Host ||
		(request.Method != http.MethodGet && request.Method != http.MethodPost) {
		return nil, errors.New("prometheus request is outside the configured source")
	}
	switch request.URL.Path {
	case t.endpoint.Path + "/api/v1/query", t.endpoint.Path + "/api/v1/query_range",
		t.endpoint.Path + "/api/v1/label/__name__/values":
	default:
		return nil, errors.New("prometheus request is not a supported read endpoint")
	}
	request = request.Clone(request.Context())
	if t.credential != "" {
		request.Header.Set("Authorization", "Bearer "+t.credential)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized ||
		(response.StatusCode == http.StatusForbidden && request.Method == http.MethodGet) {
		_ = response.Body.Close()
		return nil, errDenied
	}
	if response.ContentLength > maxResponseBytes {
		_ = response.Body.Close()
		return nil, errResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, errResponse
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

func queryError(err error) error {
	var apiErr *prom.Error
	var networkErr net.Error
	switch {
	case errors.Is(err, errDenied):
		return errDenied
	case errors.Is(err, errRedirect):
		return errRedirect
	case errors.Is(err, errResponse):
		return errResponse
	case errors.Is(err, context.Canceled):
		return errors.New("prometheus request cancelled")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkErr) && networkErr.Timeout():
		return errors.New("prometheus request timed out")
	case errors.As(err, &apiErr):
		switch apiErr.Type {
		case prom.ErrBadData:
			return errors.New("prometheus rejected the query or parameters")
		case prom.ErrTimeout, prom.ErrCanceled:
			return errors.New("prometheus query timed out or was cancelled")
		case prom.ErrExec:
			return errors.New("prometheus could not execute the query")
		case prom.ErrServer:
			return errors.New("prometheus source is unavailable or timed out")
		case prom.ErrBadResponse:
			return errors.New("prometheus returned a malformed or unsupported result")
		default:
			return errors.New("prometheus returned an invalid or unavailable response")
		}
	default:
		return errors.New("prometheus source is unavailable")
	}
}
