package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// ONE-CLICK INSTALLATION, WITH THE ASSOCIATION PROVEN RATHER THAN TRUSTED.
//
// GitHub returns an installation id in the browser's query string, and GitHub's own
// documentation says an application must not rely on it: whoever drives the browser
// chooses it. An application that binds whatever number comes back has proven nothing
// about who is connecting — which is exactly what the copy-the-id-out-of-the-URL form did.
//
// So the return trip is proven. The same round trip also carries an authorization code.
// That code is exchanged for a SHORT-LIVED USER ACCESS TOKEN, the token is used for one
// question — which installations can this authenticated person actually reach — and the
// installation is bound only if the one named is among them. The token is then discarded:
// it is not stored, not sealed, and never becomes a runtime credential. Runtime access
// stays what it already was, an installation token minted under the deployment's App.

// defaultWebURL is GitHub's browser origin: where an installation is started and where an
// authorization code is exchanged. It is not the API origin, and on GitHub Enterprise
// Server the two differ.
const defaultWebURL = "https://github.com"

// userInstallationPages bounds the walk over the installations an authenticated user can
// reach. A person who belongs to more accounts than this has other problems; the bound is
// here so a hostile or broken answer cannot make one connect attempt page forever.
const userInstallationPages = 10

// ErrNotYours reports a callback naming an installation the authenticated GitHub user
// cannot access. It is the documented attack failing, and it is a refusal rather than a
// warning: nothing is created and nothing is bound.
var ErrNotYours = errors.New(
	"the github account that authorized this cannot administer the installation the " +
		"callback named, so nothing was connected")

// ErrNotAnInstallation reports a callback that is not one: no code, or no installation id.
var ErrNotAnInstallation = errors.New("this is not a github installation callback")

// ErrExchangeRefused reports a code GitHub would not exchange — expired, already used, or
// issued for another client. Its text is ours; GitHub's own message is not repeated onward,
// because it arrives on a route a browser reached and is therefore somebody else's string.
var ErrExchangeRefused = errors.New(
	"github would not complete the authorization; start the connection again")

// Installer is the deployment's registration of its GitHub App as something a customer can
// install in one press: the App's slug, and the OAuth client the return trip is proven
// with. A deployment that registered none has no Installer, offers no connect flow, and
// keeps the configuration form.
type Installer struct {
	slug         string
	clientID     string
	clientSecret string
	webURL       string
	http         *http.Client
}

// NewInstaller builds the installation flow. Every field is required: two of three would
// offer a button that cannot finish.
func NewInstaller(slug, clientID, clientSecret, webURL string) (*Installer, error) {
	slug, clientID = strings.TrimSpace(slug), strings.TrimSpace(clientID)
	if slug == "" || clientID == "" || clientSecret == "" {
		return nil, errors.New(
			"a github installation flow needs the app's slug, client id and client secret")
	}
	if webURL == "" {
		webURL = defaultWebURL
	}
	return &Installer{
		slug:         slug,
		clientID:     clientID,
		clientSecret: clientSecret,
		webURL:       strings.TrimSuffix(webURL, "/"),
		http:         &http.Client{Timeout: requestTimeout},
	}, nil
}

// connect is what this provider contributes to the shared installation flow: where to send
// the browser, and how to prove what comes back.
func connect(installer *Installer, app *App, client *Client) *integrations.Connect {
	if installer == nil || !app.Configured() {
		// No flow without both halves. The App credential is what verifies the
		// installation immediately after it is bound, and a connect that could not
		// verify would land a customer on an integration nothing had checked.
		return nil
	}
	return &integrations.Connect{
		Authorize: installer.authorize,
		Redeem: func(ctx context.Context, returned integrations.ConnectReturn) (
			integrations.ConnectBinding, error,
		) {
			return installer.redeem(ctx, client, returned)
		},
	}
}

// authorize is GitHub's own installation screen. Account selection, repository selection
// and permission consent all happen there — this product never asks for any of them,
// because the choice belongs where the permissions live.
func (i *Installer) authorize(state, callback string) (string, error) {
	if state == "" || callback == "" {
		return "", errors.New("a github installation needs a state and a callback")
	}
	parameters := url.Values{"state": {state}}
	return i.webURL + "/apps/" + url.PathEscape(i.slug) + "/installations/new?" +
		parameters.Encode(), nil
}

// redeem proves the return trip and reports what to record.
//
// setup_action is not branched on. GitHub sends `install` for a new installation and
// `update` for one the customer changed, and both are answered the same way — prove
// access, then let the caller re-verify what already exists or create what does not.
// Branching on it would be trusting a query parameter to decide whether to write.
func (i *Installer) redeem(
	ctx context.Context, client *Client, returned integrations.ConnectReturn,
) (integrations.ConnectBinding, error) {
	// Nothing else in the query is read. An organization identifier here is not consulted
	// by anything: the tenant comes from the flow this state redeemed.
	code := strings.TrimSpace(returned.Query.Get("code"))
	installation, err := installationFromCallback(returned.Query.Get("installation_id"))
	if err != nil || code == "" {
		countCheck(ctx, "not-an-installation", "refused")
		return integrations.ConnectBinding{}, ErrNotAnInstallation
	}

	token, err := i.exchange(ctx, code, returned.Callback)
	if err != nil {
		// The commonest shape of a misregistered App: the deployment's OAuth client is
		// wrong and every connect dies here, which is a number rather than a mystery.
		countCheck(ctx, "exchange-refused", "refused")
		return integrations.ConnectBinding{}, err
	}
	// The token exists for this one question and for nothing else. It is never returned,
	// never stored, and no read is made with it beyond the installations listing below.
	account, err := i.prove(ctx, client, token, installation)
	if err != nil {
		countCheck(ctx, "not-administered", "refused")
		return integrations.ConnectBinding{}, err
	}
	countCheck(ctx, "proven", "proven")

	return integrations.ConnectBinding{
		Name: "GitHub — " + account,
		// The installation id is recorded as a JSON number, which is what it is after a
		// round trip through the configuration column.
		Configuration: map[string]any{"installationId": float64(installation)},
	}, nil
}

// installationFromCallback reads the id the browser carried. It is validated as a whole
// positive number and then PROVEN; parsing it says nothing about whose it is.
func installationFromCallback(value string) (int64, error) {
	installation, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || installation < 1 {
		return 0, ErrNotAnInstallation
	}
	return installation, nil
}

// exchange turns the authorization code into a short-lived user access token.
//
// The request carries the deployment's client secret, so nothing about it is logged and no
// error quotes the body. GitHub answers 200 with an error field rather than a status for a
// refused code, which is why the body is read rather than the status.
func (i *Installer) exchange(ctx context.Context, code, callback string) (string, error) {
	form := url.Values{
		"client_id":     {i.clientID},
		"client_secret": {i.clientSecret},
		"code":          {code},
		"redirect_uri":  {callback},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		i.webURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", ErrExchangeRefused
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := i.http.Do(request)
	if err != nil {
		// url.Error carries the URL and the form is in the body; neither is quoted on.
		return "", fmt.Errorf("%w: github could not be reached", ErrExchangeRefused)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, exchangeResponseBound))
	if err != nil {
		return "", ErrExchangeRefused
	}
	var decoded struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.AccessToken == "" {
		return "", ErrExchangeRefused
	}
	return decoded.AccessToken, nil
}

// exchangeResponseBound is what one token answer may hold. A token response is a few
// hundred bytes; anything approaching this is not the endpoint this speaks to.
const exchangeResponseBound = 16 << 10

// prove is the check this whole flow exists for: GitHub is asked which installations the
// authenticated person can actually administer, and the callback's installation id is
// refused unless it is among them.
//
// The account login it returns comes from GitHub's answer to that question rather than from
// the callback, so the name recorded is one the far end asserted.
func (i *Installer) prove(
	ctx context.Context, client *Client, token string, installation int64,
) (string, error) {
	for page := 1; page <= userInstallationPages; page++ {
		reachable, more, err := client.UserInstallations(ctx, token, page)
		if err != nil {
			return "", fmt.Errorf(
				"%w: github would not say which installations this account can administer",
				ErrExchangeRefused)
		}
		for _, one := range reachable {
			if one.ID == installation {
				return one.Account, nil
			}
		}
		if !more {
			break
		}
	}
	return "", ErrNotYours
}
