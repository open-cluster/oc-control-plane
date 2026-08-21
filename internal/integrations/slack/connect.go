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

// ONE-CLICK INSTALLATION, AND WHY IT IS THE ONLY PATH WORTH HAVING.
//
// Connecting Slack used to mean the customer created their own Slack app, chose scopes
// from our documentation, installed it, copied an xoxb- token out of Slack's settings and
// pasted it into a form. That is a workshop procedure, and it is the first thing a design
// partner sees. Worse, it puts a live credential through somebody's clipboard.
//
// Here the deployment registers ONE Slack app and the customer presses a button. The
// credential is exchanged server-side and never touches the browser.
//
// The pasted form is not deleted. A deployment that registered no Slack app still serves
// it, because an air-gapped install has no other way in — and every integration already
// connected that way keeps working untouched, unmigrated and unre-verified. There is one
// Slack integration with one client, one tool set and one set of capabilities; how the
// credential was obtained is the only thing that differs.

// TeamIDField is where the installed workspace's own identifier is recorded.
//
// Non-secret, and load-bearing twice over. It is the identity a repeat connection is
// recognised by, so reconnecting a workspace RE-VERIFIES the integration that exists
// instead of silently creating a second one beside it. And its presence is what
// distinguishes an app installation from a pasted token: a pasted token names no
// installation, so nothing can route an inbound event to it.
const TeamIDField = "teamId"

// AppIDField is the app the workspace installed. Recorded beside the team because the
// resolution key for an inbound event is the app AND the workspace: one deployment may
// serve more than one app registration over its life, and a team id alone would collide.
const AppIDField = "appID"

// requestedScopes is what the app asks a workspace for. Least privilege, and every entry
// is here for a reason a customer could be told.
//
// search:read IS NOT REQUESTED. The security story is that OpenCluster reasons over
// conversations it has deliberately been invited into, not everything an employee can see.
// If workspace-wide search is ever worth having it becomes an explicit elevated capability,
// re-authorized deliberately — not a scope that arrived quietly with everything else.
//
// VERIFY THESE AGAINST SLACK'S CURRENT DOCUMENTATION before a release. Slack's agent
// platform is moving quickly, and a scope name that has been renamed fails at install time
// in front of a customer.
var requestedScopes = []string{
	// The agent capability itself: the app appears as an agent and the native streaming
	// methods become usable.
	"assistant:write",
	// Post and stream the reply.
	"chat:write",
	// Receive @OpenCluster in channels the app is in.
	"app_mentions:read",
	// Read the agent DM the engineer is talking to.
	"im:history",
	// Read the public channel or thread OpenCluster was asked about.
	"channels:history",
	// Resolve and list public channels.
	"channels:read",
	// Resolve authors to names, for display and for audit.
	"users:read",
	// Private channels the app has been explicitly invited to.
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

// ErrNotABotInstall reports an exchange that returned no bot token. Every capability this
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
		Authorize:       installer.authorize,
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
	Enterprise   bool
	BotUserID    string
	AuthedUserID string
	Scopes       []string
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
	return integrations.ConnectBinding{
		Name:       "Slack — " + name,
		Credential: token,
		Configuration: map[string]any{
			TeamIDField: installed.TeamID,
			AppIDField:  installed.AppID,
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
		AppID:        decoded.AppID,
		TeamID:       decoded.Team.ID,
		TeamName:     decoded.Team.Name,
		Enterprise:   decoded.IsEnterpriseInstall,
		BotUserID:    decoded.BotUserID,
		AuthedUserID: decoded.AuthedUser.ID,
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
