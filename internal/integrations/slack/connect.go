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

var requestedScopes = []string{
	"assistant:write",
	"chat:write",
	"app_mentions:read",
	"channels:history",
	"channels:read",
	"users:read",
	"groups:history",
}

var ErrNotAnInstallation = errors.New("this is not a slack installation callback")

var ErrExchangeRefused = errors.New(
	"slack would not complete the authorization; start the connection again")

var ErrNotABotInstall = errors.New(
	"slack returned no bot token for this installation, so nothing was connected")

type Installer struct {
	clientID     string
	clientSecret string
	apiURL       string
	http         *http.Client
}

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

func connect(installer *Installer, client *Client) *integrations.Connect {
	if installer == nil {
		return nil
	}
	return &integrations.Connect{
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

func browserOrigin(apiURL string) string {
	return strings.TrimSuffix(strings.TrimSuffix(apiURL, "/"), "/api")
}

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

type installation struct {
	AppID        string
	TeamID       string
	TeamName     string
	EnterpriseID string
	BotUserID    string
	Scopes       []string
}

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

	identity, err := client.AuthTest(ctx, token)
	if err != nil {
		return integrations.ConnectBinding{}, ErrExchangeRefused
	}

	name := identity.Workspace
	if name == "" {
		name = installed.TeamName
	}
	agent := identity.BotUserID
	if agent == "" {
		agent = installed.BotUserID
	}
	return integrations.ConnectBinding{
		Name:          "Slack — " + name,
		Credential:    token,
		Configuration: map[string]any{},
		Installation: &integrations.Installation{
			Key: integrations.InstallationKey(providerInstallationKey(
				installed.AppID, installed.EnterpriseID, installed.TeamID)),
			ProviderActorID: agent,
		},
	}, nil
}

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
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).
		Decode(&decoded); err != nil || !decoded.OK {
		return "", installation{}, ErrExchangeRefused
	}
	if decoded.AccessToken == "" || decoded.Team.ID == "" {
		return "", installation{}, ErrNotABotInstall
	}

	installed := installation{
		AppID:     decoded.AppID,
		TeamID:    decoded.Team.ID,
		TeamName:  decoded.Team.Name,
		BotUserID: decoded.BotUserID,
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
