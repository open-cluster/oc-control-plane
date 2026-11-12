package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
		Authorize: func(ctx context.Context, state, callback string) (string, error) {
			if state == "" || callback == "" {
				return "", errors.New("a github installation needs state and callback URLs")
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
		AuthorizeExisting: func(_ context.Context, state, callback, owner string) (string, string, error) {
			if state == "" || callback == "" {
				return "", "", errors.New("github authorization needs state and a callback URL")
			}
			if owner != "" {
				// The single-use flow stores the digest of the full state, binding the owner to this attempt.
				state += "." + ownerDigest(owner)
			}
			parameters := url.Values{
				"client_id":    {clientID},
				"prompt":       {"select_account"},
				"redirect_uri": {callback},
				"state":        {state},
			}
			return webURL + "/login/oauth/authorize?" + parameters.Encode(), state, nil
		},
		Redeem: func(ctx context.Context, returned integrations.ConnectReturn) (
			integrations.ConnectBinding, error,
		) {
			installationIDs := returned.Query["installation_id"]
			if len(installationIDs) > 1 {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			var requestedID int64
			if len(installationIDs) == 1 {
				var err error
				requestedID, err = strconv.ParseInt(strings.TrimSpace(installationIDs[0]), 10, 64)
				if err != nil || requestedID < 1 {
					return integrations.ConnectBinding{}, errInvalidInstallation
				}
			}
			userToken, err := exchangeUserCode(ctx, client, clientID, clientSecret, returned.Query.Get("code"), returned.Callback)
			if err != nil {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			_, selectedOwner, _ := strings.Cut(returned.Query.Get("state"), ".")
			id, installed, err := authorizedInstallation(ctx, app, client, userToken, requestedID, selectedOwner)
			if err != nil {
				return integrations.ConnectBinding{}, err
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

func ownerDigest(owner string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(owner)))
	return hex.EncodeToString(digest[:])
}

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

func authorizedInstallation(
	ctx context.Context, app *App, client *Client, userToken string, requestedID int64, selectedOwner string,
) (int64, Installation, error) {
	jwt, err := app.jwt(time.Now())
	if err != nil {
		return 0, Installation{}, err
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if _, err := client.call(ctx, userToken, http.MethodGet, "/user", nil, &user); err != nil || user.ID <= 0 {
		return 0, Installation{}, errInvalidInstallation
	}
	var selectedID int64
	var selected Installation
	for page := 1; page <= authorizationPageLimit; page++ {
		installations, next, err := client.UserInstallations(ctx, userToken, page)
		if err != nil {
			return 0, Installation{}, err
		}
		for _, installation := range installations {
			if installation.ID < 1 {
				return 0, Installation{}, errInvalidInstallation
			}
			if installation.ID == selectedID || (requestedID != 0 && installation.ID != requestedID) {
				continue
			}
			providerInstallation, err := client.Installation(ctx, jwt, installation.ID)
			if err != nil {
				return 0, Installation{}, err
			}
			if providerInstallation.Suspended ||
				(selectedOwner != "" && ownerDigest(providerInstallation.Account) != selectedOwner) {
				continue
			}
			owned, err := userOwnsInstallation(ctx, client, userToken, user.ID, providerInstallation)
			if err != nil {
				return 0, Installation{}, err
			}
			if !owned {
				continue
			}
			if requestedID != 0 {
				return installation.ID, providerInstallation, nil
			}
			if selectedID != 0 {
				return 0, Installation{}, integrations.ErrConnectAmbiguousInstallation
			}
			selectedID, selected = installation.ID, providerInstallation
		}
		if !next {
			if selectedID != 0 {
				return selectedID, selected, nil
			}
			break
		}
	}
	return 0, Installation{}, errInvalidInstallation
}

func userOwnsInstallation(
	ctx context.Context, client *Client, userToken string, userID int64, providerInstallation Installation,
) (bool, error) {
	if providerInstallation.AccountID <= 0 {
		return false, errInvalidInstallation
	}
	if providerInstallation.AccountType == "User" {
		return userID == providerInstallation.AccountID, nil
	}
	if providerInstallation.AccountType != "Organization" || providerInstallation.Account == "" {
		return false, errInvalidInstallation
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
	if err != nil || membership.Organization.ID != providerInstallation.AccountID {
		return false, errInvalidInstallation
	}
	return membership.State == "active" && membership.Role == "admin", nil
}
