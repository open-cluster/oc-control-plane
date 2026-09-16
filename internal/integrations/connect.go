package integrations

import (
	"context"
	"errors"
	"net/url"
	"time"
)

var ErrConnectFlowUnknown = errors.New("connect flow unknown")

const connectFlowLifetime = 15 * time.Minute

type ConnectFlow struct {
	Organization string
	Provider     Provider
	Principal    string
	ReturnTo     string
	ExpiresAt    time.Time
}

type Connect struct {
	// SealsCredential lets the handler refuse before authorization, when no sealer exists.
	SealsCredential bool
	Authorize       func(ctx context.Context, state, callback string) (string, error)
	Redeem          func(ctx context.Context, returned ConnectReturn) (ConnectBinding, error)
}

type ConnectReturn struct {
	Query    url.Values
	Callback string
}

type ConnectBinding struct {
	Credential    string
	Name          string
	Configuration map[string]any
	Installation  *Installation
}

// Connectable reports whether this definition offers a provider installation flow.
func (d Definition) Connectable() bool {
	return d.Connect != nil && d.Connect.Authorize != nil && d.Connect.Redeem != nil
}
