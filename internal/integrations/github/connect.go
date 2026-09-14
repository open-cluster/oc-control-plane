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

func connect(app *App, client *Client) *integrations.Connect {
	webURL := browserOrigin(client)
	if !app.Configured() || webURL == "" {
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
		Redeem: func(ctx context.Context, returned integrations.ConnectReturn) (
			integrations.ConnectBinding, error,
		) {
			id, err := strconv.ParseInt(strings.TrimSpace(returned.Query.Get("installation_id")), 10, 64)
			if err != nil || id < 1 {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			jwt, err := app.jwt(time.Now())
			if err != nil {
				return integrations.ConnectBinding{}, err
			}
			installed, err := client.Installation(ctx, jwt, id)
			if err != nil || installed.Suspended {
				return integrations.ConnectBinding{}, errInvalidInstallation
			}
			return integrations.ConnectBinding{
				Name:          "GitHub — " + installed.Account,
				Configuration: map[string]any{},
				Installation: &integrations.Installation{
					Application: "github",
					Workspace:   strconv.FormatInt(id, 10),
					Agent:       installed.Account,
				},
			}, nil
		},
	}
}
