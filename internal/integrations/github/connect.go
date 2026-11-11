package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

var errInvalidInstallation = errors.New("github did not return a usable installation")

func connect(app *App, client *Client, clientID, clientSecret string) *integrations.Connect {
	webURL := browserOrigin(client)
	if !app.Configured() || webURL == "" || clientID == "" || clientSecret == "" {
		return nil
	}
	return &integrations.Connect{
		Authorize: func(ctx context.Context, state, callback, providerInstallationID string) (string, error) {
			if state == "" || callback == "" {
				return "", errors.New("a github installation needs state and callback URLs")
			}
			if providerInstallationID != "" {
				id, err := strconv.ParseInt(providerInstallationID, 10, 64)
				if err != nil || id <= 0 {
					return "", integrations.ErrInvalidConnectSelection
				}
				parameters := url.Values{
					"client_id":    {clientID},
					"redirect_uri": {callback},
					"state":        {state},
				}
				return webURL + "/login/oauth/authorize?" + parameters.Encode(), nil
			}

			jwt, err := app.jwt(time.Now())
			if err != nil {
				return "", err
			}
			slug, err := client.App(ctx, jwt)
			if err != nil {
				return "", err
			}
			return webURL + "/apps/" + url.PathEscape(slug) + "/installations/new?" +
				url.Values{"state": {state}}.Encode(), nil
		},
		Redeem: func(ctx context.Context, returned integrations.ConnectReturn) (
			integrations.ConnectBinding, error,
		) {
			providerInstallationID := returned.ProviderInstallationID
			if providerInstallationID == "" {
				selections := returned.Query["installation_id"]
				if len(selections) != 1 {
					return integrations.ConnectBinding{}, errInvalidInstallation
				}
				providerInstallationID = selections[0]
			}
			id, err := strconv.ParseInt(strings.TrimSpace(providerInstallationID), 10, 64)
			if err != nil || id < 1 {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			userToken, err := exchangeUserCode(ctx, client, clientID, clientSecret, returned.Query.Get("code"), returned.Callback)
			if err != nil {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			jwt, err := app.jwt(time.Now())
			if err != nil {
				return integrations.ConnectBinding{}, err
			}
			installed, err := client.Installation(ctx, jwt, id)
			if err != nil || installed.Suspended || !userOwnsInstallation(ctx, client, userToken, id, installed) {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			return integrations.ConnectBinding{
				Name:          "GitHub — " + installed.Account,
				Configuration: map[string]any{},
				Installation: &integrations.Installation{
					Key: integrations.InstallationKey{strconv.FormatInt(id, 10)},
				},
			}, nil
		},
	}
}

const authorizationPageLimit = 10

func exchangeUserCode(ctx context.Context, client *Client, clientID, clientSecret, code, callback string) (string, error) {
	if code == "" || callback == "" {
		return "", errInvalidInstallation
	}
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {callback},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, browserOrigin(client)+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errInvalidInstallation
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	httpClient := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("OAuth redirects are refused")
		},
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return "", errInvalidInstallation
	}
	defer func() { _ = response.Body.Close() }()
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&token) != nil ||
		token.Error != "" || token.AccessToken == "" || token.TokenType != "bearer" {
		return "", errInvalidInstallation
	}
	return token.AccessToken, nil
}

func userOwnsInstallation(ctx context.Context, client *Client, userToken string, installationID int64, providerInstallation Installation) bool {
	associated := false
	for page := 1; page <= authorizationPageLimit; page++ {
		installations, next, err := client.UserInstallations(ctx, userToken, page)
		if err != nil {
			return false
		}
		for _, installation := range installations {
			if installation.ID == installationID {
				associated = true
				break
			}
		}
		if associated || !next {
			break
		}
	}
	if !associated || providerInstallation.AccountID <= 0 {
		return false
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if _, err := client.call(ctx, userToken, http.MethodGet, "/user", nil, &user); err != nil || user.ID <= 0 {
		return false
	}
	if providerInstallation.AccountType == "User" {
		return user.ID == providerInstallation.AccountID
	}
	if providerInstallation.AccountType != "Organization" || providerInstallation.Account == "" {
		return false
	}
	var membership struct {
		State        string `json:"state"`
		Role         string `json:"role"`
		Organization struct {
			ID int64 `json:"id"`
		} `json:"organization"`
	}
	_, err := client.call(ctx, userToken, http.MethodGet,
		"/user/memberships/orgs/"+url.PathEscape(providerInstallation.Account), nil, &membership)
	return err == nil && membership.Organization.ID == providerInstallation.AccountID &&
		membership.State == "active" && membership.Role == "admin"
}
