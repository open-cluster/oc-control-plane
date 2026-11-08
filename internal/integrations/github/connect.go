package github

import (
	"context"
	"errors"
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
					return "", errInvalidInstallation
				}
				return webURL + "/login/oauth/authorize?" + url.Values{"client_id": {clientID}, "redirect_uri": {callback}, "state": {state}}.Encode(), nil
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
			id, err := strconv.ParseInt(strings.TrimSpace(returned.ProviderInstallationID), 10, 64)
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
