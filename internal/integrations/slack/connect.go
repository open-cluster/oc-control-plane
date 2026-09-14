package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// requestedScopes is the least-privilege bot grant for connection and inbound mentions.
// search:read IS NOT REQUESTED. The security story is that OpenCluster reasons over
// conversations it has deliberately been invited into, not everything an employee can see.
// If workspace-wide search is ever worth having it becomes an explicit elevated Tool,
// re-authorized deliberately — not a scope that arrived quietly with everything else.
//
// VERIFY THESE AGAINST SLACK'S CURRENT DOCUMENTATION before a release. Slack's agent
// platform is moving quickly, and a scope name that has been renamed fails at install time
// in front of a customer.
var requestedScopes = []string{
	"assistant:write",
	"chat:write",
	"app_mentions:read",
	"channels:history",
	"channels:read",
	"users:read",
	"groups:history",
}

// ErrNotAnInstallation reports a callback that is not one: no authorization code.
var ErrNotAnInstallation = errors.New("this is not a slack installation callback")

// ErrExchangeRefused reports a code Slack would not exchange — expired, already used, or
// issued for another client. Its text is OURS. Slack's own message arrives on a route a
// browser reached, which makes it somebody else's string, and repeating it onward would put
// attacker-influenced text in front of an operator.
var ErrExchangeRefused = errors.New(
	"slack would not complete the authorization; start the connection again")

// ErrNotABotInstall reports an exchange that returned no bot token. Every Tool this
// integration offers is the bot's, so a user-token-only grant is an installation that
// cannot do the thing it was installed for.
var ErrNotABotInstall = errors.New(
	"slack returned no bot token for this installation, so nothing was connected")

// Installer is the deployment's registration of the OpenCluster Slack app: the OAuth
// client an installation is exchanged through. A deployment that registered none has no
// Installer, offers no connect flow, and keeps the configuration form.
type Installer struct {
	clientID     string
	clientSecret string
	apiURL       string
	http         *http.Client
}

// NewInstaller builds the installation flow. Both halves are required: one of two would
// offer a button that cannot finish.
func NewInstaller(clientID, clientSecret, apiURL string) (*Installer, error) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || clientSecret == "" {
		return nil, errors.New(
			"a slack installation flow needs the app's client id and client secret")
	}
	if apiURL == "" {
		apiURL = defaultBaseURL
	}
	return &Installer{
		clientID:     clientID,
		clientSecret: clientSecret,
		apiURL:       strings.TrimSuffix(apiURL, "/"),
		http:         &http.Client{Timeout: requestTimeout},
	}, nil
}

// connect is what this provider contributes to the shared installation flow.
func connect(installer *Installer, client *Client) *integrations.Connect {
	if installer == nil {
		return nil
	}
	return &integrations.Connect{
		// The flow comes back holding the bot token, so a deployment that cannot seal
		// must refuse before the browser is sent anywhere. Saying so here is what makes
		// that refusal happen at the start rather than after the customer has granted
		// real permissions in their own workspace.
		SealsCredential: true,
		Authorize: func(_ context.Context, state, callback string) (string, error) {
			return installer.authorize(state, callback)
		},
		Redeem: func(ctx context.Context, returned integrations.ConnectReturn) (
			integrations.ConnectBinding, error,
		) {
			return installer.redeem(ctx, client, returned)
		},
	}
}

// browserOrigin is where a person authorizes, as against where the API is called.
//
// Slack serves the two from one host under different paths — https://slack.com/api for
// calls and https://slack.com/oauth/... for the install screen — so the browser origin is
// derived by dropping the API path segment rather than configured separately. A test
// pointing the API at a fake gets that fake's origin, which is what lets the whole flow run
// against one scripted server.
func browserOrigin(apiURL string) string {
	return strings.TrimSuffix(strings.TrimSuffix(apiURL, "/"), "/api")
}

// authorize is Slack's own installation screen. Workspace selection and permission consent
// happen there, where the permissions live; this product never asks for either.
func (i *Installer) authorize(state, callback string) (string, error) {
	if state == "" || callback == "" {
		return "", errors.New("a slack installation needs a state and a callback")
	}
	parameters := url.Values{
		"client_id":    {i.clientID},
		"scope":        {strings.Join(requestedScopes, ",")},
		"state":        {state},
		"redirect_uri": {callback},
	}
	return browserOrigin(i.apiURL) + "/oauth/v2/authorize?" + parameters.Encode(), nil
}

// installation is what an exchange established. Everything here is non-secret except the
// bot token, which travels separately and is never logged.
type installation struct {
	AppID        string
	TeamID       string
	TeamName     string
	EnterpriseID string
	// EnterpriseWide is the vendor's own flag, not a value derived from the enterprise id.
	EnterpriseWide bool
	BotUserID      string
	AuthedUserID   string
	Scopes         []string
}

// redeem exchanges the code for the workspace's bot token and reports what to record.
//
// Nothing else in the query is read. An organization identifier arriving here is not
// consulted by anything: the tenant comes from the flow the state redeemed, which is the
// property that makes a tampered callback bind nothing.
func (i *Installer) redeem(
	ctx context.Context, client *Client, returned integrations.ConnectReturn,
) (integrations.ConnectBinding, error) {
	code := strings.TrimSpace(returned.Query.Get("code"))
	if code == "" {
		return integrations.ConnectBinding{}, ErrNotAnInstallation
	}

	token, installed, err := i.exchange(ctx, code, returned.Callback)
	if err != nil {
		return integrations.ConnectBinding{}, err
	}

	// Proven against the workspace before anything is recorded. The exchange says what
	// Slack believes; auth.test is the far end answering as this bot, which is the only
	// check that survives a stale or partially-revoked install.
	identity, err := client.AuthTest(ctx, token)
	if err != nil {
		return integrations.ConnectBinding{}, ErrExchangeRefused
	}

	name := identity.Workspace
	if name == "" {
		name = installed.TeamName
	}
	// The bot's own identity comes from auth.test rather than from the exchange, because
	// auth.test is the far end answering as this bot right now. It is what stops the agent
	// replying to its own messages, so a stale value would be a loop.
	agent := identity.BotUserID
	if agent == "" {
		agent = installed.BotUserID
	}
	return integrations.ConnectBinding{
		Name:          "Slack — " + name,
		Credential:    token,
		Configuration: map[string]any{},
		// The routing record, written in the same transaction as the Integration. Without
		// it the integration exists and no event can reach it, which is a customer who
		// pressed Connect, authorized, and has an agent that never answers.
		Installation: &integrations.Installation{
			Application:    installed.AppID,
			Enterprise:     installed.EnterpriseID,
			EnterpriseWide: installed.EnterpriseWide,
			Workspace:      installed.TeamID,
			Agent:          agent,
			Authorizer:     installed.AuthedUserID,
		},
	}, nil
}

// exchange trades the authorization code for the workspace's bot token, server-side.
//
// The client secret is presented in the POST body over TLS, which is what Slack's own
// documentation specifies for this call. The code is single-use at Slack's end; a replay
// is refused there and reaches this process as a refusal to exchange.
func (i *Installer) exchange(
	ctx context.Context, code, callback string,
) (string, installation, error) {
	form := url.Values{
		"client_id":     {i.clientID},
		"client_secret": {i.clientSecret},
		"code":          {code},
		"redirect_uri":  {callback},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		i.apiURL+"/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return "", installation{}, fmt.Errorf("building the slack token exchange: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	response, err := i.http.Do(request)
	if err != nil {
		return "", installation{}, ErrExchangeRefused
	}
	defer func() { _ = response.Body.Close() }()

	var decoded struct {
		OK          bool   `json:"ok"`
		Error       string `json:"error"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		AppID       string `json:"app_id"`
		BotUserID   string `json:"bot_user_id"`
		Team        struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		Enterprise *struct {
			ID string `json:"id"`
		} `json:"enterprise"`
		IsEnterpriseInstall bool `json:"is_enterprise_install"`
		AuthedUser          struct {
			ID string `json:"id"`
		} `json:"authed_user"`
	}
	// Bounded like every other read of a vendor answer: this is reached by a browser and
	// the far end is not this deployment's to trust with an unbounded body.
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).
		Decode(&decoded); err != nil || !decoded.OK {
		return "", installation{}, ErrExchangeRefused
	}
	if decoded.AccessToken == "" || decoded.Team.ID == "" {
		return "", installation{}, ErrNotABotInstall
	}

	installed := installation{
		AppID:          decoded.AppID,
		TeamID:         decoded.Team.ID,
		TeamName:       decoded.Team.Name,
		EnterpriseWide: decoded.IsEnterpriseInstall,
		BotUserID:      decoded.BotUserID,
		AuthedUserID:   decoded.AuthedUser.ID,
	}
	if decoded.Enterprise != nil {
		installed.EnterpriseID = decoded.Enterprise.ID
	}
	for scope := range strings.SplitSeq(decoded.Scope, ",") {
		if trimmed := strings.TrimSpace(scope); trimmed != "" {
			installed.Scopes = append(installed.Scopes, trimmed)
		}
	}
	return decoded.AccessToken, installed, nil
}
