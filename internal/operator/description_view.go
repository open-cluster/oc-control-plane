package operator

import (
	"net/http"

	"github.com/open-cluster/oc-control-plane/internal/describe"
)

// What the self-description says on the wire. Kept apart from the assembly for the reason
// every other view in this repository is: it is a contract, and a field renamed here is a
// client broken somewhere else.

// Describe is the operator package's own contribution: the relay listings it serves
// directly.
//
// None of its writes takes a body — clearing a conflict and minting an enrolment token are
// both decided entirely by the path and the caller — so it declares none, which is a
// statement about this surface rather than a gap in the document.
func (h Handlers) Describe() describe.Contribution {
	const relays = "/operator/v1/organizations/{organization}/relays"

	return describe.Contribution{
		Listings: []describe.Listing{
			{Route: http.MethodGet + " " + relays, Spec: fleetSpec},
			{
				Route: http.MethodGet + " " + relays + "/{registration}/integrations",
				Spec:  relayIntegrationsSpec,
			},
			{
				Route: http.MethodGet + " " + relays + "/{registration}/failures",
				Spec:  relayFailuresSpec,
			},
		},
	}
}

// descriptionView is the served document.
type descriptionView struct {
	// Surfaces are the optional parts of the product, each with whether this deployment
	// serves it. Present so that a 404 is never how a caller discovers what exists.
	Surfaces []surfaceView `json:"surfaces"`
	// Routes is every route this deployment serves, with what reaching it requires.
	Routes []routeView `json:"routes"`
	// Listings is how each listing may be queried, and ListingRules are the conventions
	// they all share.
	Listings     []listingView `json:"listings"`
	ListingRules rulesView     `json:"listingRules"`
	// RequestBodies is what each write accepts. Every write refuses a field it does not
	// declare, so this is the contract behind that refusal rather than a surprise a client
	// meets one 400 at a time.
	RequestBodies []bodyView `json:"requestBodies"`
}

type surfaceView struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Note    string `json:"note"`
}

type routeView struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Access is public, authenticated or privileged.
	Access string `json:"access"`
	// Permission is what a privileged route requires, and absent on anything else —
	// absent rather than empty, because "this route needs no permission" and "somebody
	// forgot to say" would otherwise read the same.
	Permission string `json:"permission,omitempty"`
}

type listingView struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Searchable says whether this listing accepts free text. One that does not REFUSES
	// `search` rather than ignoring it, which is why it is stated.
	Searchable bool     `json:"searchable"`
	Sortable   []string `json:"sortable"`
	// DefaultSort is what is applied when the caller names none, in the same signed form a
	// caller would send it back as.
	DefaultSort string   `json:"defaultSort"`
	Filters     []string `json:"filters"`
}

type rulesView struct {
	Reserved     []string `json:"reservedParameters"`
	SortPrefixes []string `json:"sortPrefixes"`
	DefaultLimit int      `json:"defaultLimit"`
	MaxLimit     int      `json:"maxLimit"`
	// LimitIsClamped says a limit above the ceiling is reduced rather than refused.
	LimitIsClamped bool `json:"limitIsClamped"`
}

type bodyView struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Schema is JSON Schema, in the dialect the integration configuration already serves,
	// reflected from the type the handler decodes into.
	Schema map[string]any `json:"schema"`
}

func descriptionViewOf(document describe.Document) descriptionView {
	view := descriptionView{
		Surfaces:      make([]surfaceView, 0, len(document.Surfaces)),
		Routes:        make([]routeView, 0, len(document.Routes)),
		Listings:      make([]listingView, 0, len(document.Listings)),
		RequestBodies: make([]bodyView, 0, len(document.Bodies)),
		ListingRules: rulesView{
			Reserved:       document.Rules.Reserved,
			SortPrefixes:   document.Rules.SortPrefixes,
			DefaultLimit:   document.Rules.DefaultLimit,
			MaxLimit:       document.Rules.MaxLimit,
			LimitIsClamped: document.Rules.LimitIsClamped,
		},
	}
	for _, surface := range document.Surfaces {
		view.Surfaces = append(view.Surfaces, surfaceView(surface))
	}
	for _, route := range document.Routes {
		view.Routes = append(view.Routes, routeView(route))
	}
	for _, listing := range document.Listings {
		view.Listings = append(view.Listings, listingView(listing))
	}
	for _, body := range document.Bodies {
		view.RequestBodies = append(view.RequestBodies, bodyView(body))
	}
	return view
}
