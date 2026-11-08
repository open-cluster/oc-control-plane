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
)

const authorizationPageLimit = 10

func exchangeUserCode(ctx context.Context, client *Client, clientID, clientSecret, code, callback string) (string, error) {
	if code == "" || callback == "" {
		return "", errInvalidInstallation
	}
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}, "redirect_uri": {callback}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, browserOrigin(client)+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", errInvalidInstallation
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	httpClient := &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OAuth redirects are refused") }}
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
	if providerInstallation.AccountType != "Organization" {
		return false
	}
	for page := 1; page <= authorizationPageLimit; page++ {
		var memberships []struct {
			State        string `json:"state"`
			Role         string `json:"role"`
			Organization struct {
				ID int64 `json:"id"`
			} `json:"organization"`
		}
		header, err := client.call(ctx, userToken, http.MethodGet, "/user/memberships/orgs", url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}, "state": {"active"}}, &memberships)
		if err != nil {
			return false
		}
		for _, membership := range memberships {
			if membership.Organization.ID == providerInstallation.AccountID && membership.State == "active" && membership.Role == "admin" {
				return true
			}
		}
		if !hasNextPage(header) {
			return false
		}
	}
	return false
}
