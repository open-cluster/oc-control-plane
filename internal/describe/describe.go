// Package describe holds what this deployment says it offers, and the vocabulary each
// capability contributes it in.
//
// It exists because a console could not learn what a deployment serves. Conversations are
// off by default and answer 404 when off — deliberately, and correctly, because a
// deployment with them off does not have that surface. But nothing said WHICH surfaces
// exist, so a console deciding whether "Investigate this incident" opens a Conversation or
// a single-shot Investigation could only probe and interpret a 404, which is guessing with
// better manners. Every listing already declared its filters, its sortable fields, its
// default sort and whether it is searchable, and every write already refused a field it did
// not declare; none of it was served, so a client built its own dialect and its suite went
// green over requests this service answers 400.
//
// NOTHING HERE IS WRITTEN DOWN TWICE. A document maintained beside the handlers would be
// another copy of the contract, and drift between copies is the bug this exists to end.
// What a listing accepts is the same Spec value its handler parses with; what a body
// accepts is reflected from the type its handler decodes into; the routes and the
// permissions are the route table itself.
//
// Like internal/table, this package is vocabulary and assembly: no I/O, no database, no
// HTTP writing, and no knowledge of any particular capability.
package describe

import (
	"sort"

	"github.com/open-cluster/oc-control-plane/internal/authz"
	"github.com/open-cluster/oc-control-plane/internal/table"
)

// Contribution is what ONE capability says about itself, beside the routes it already
// contributes. A capability with no listings and no request bodies contributes the zero
// value, which is a statement rather than an omission.
type Contribution struct {
	Listings []Listing
	Bodies   []Body
	// Surfaces are the optional surfaces this capability owns, each with whether this
	// deployment serves it. A capability that is always served declares none.
	Surfaces []Surface
}

// Listing binds one listing's contract to the route that serves it.
//
// Spec is the SAME value the handler passes to table.Parse. Anything else would be a second
// copy of the answer to "what may I filter this by", and the first thing a second copy does
// is disagree with the first.
type Listing struct {
	// Route is the route's key — method and pattern, exactly as the route table spells it —
	// so a gate can hold the two against each other by string equality.
	Route string
	Spec  table.Spec
}

// Body binds one request body's shape to the route that accepts it.
//
// Example is a zero value of the type the handler decodes into, and the schema is reflected
// from it. Naming the TYPE rather than writing a schema is what makes the document unable to
// drift from the decoder: a field renamed in Go is a field renamed in the document, in the
// same commit, with no second edit to forget.
type Body struct {
	Route   string
	Example any
}

// Surface is one optional part of the product, and whether this deployment serves it.
//
// Named rather than inferred. A caller that had to discover Conversations by asking for one
// and reading a 404 cannot tell "this deployment does not serve them" from "that identifier
// was wrong" or "this build is older than the feature".
type Surface struct {
	Name string
	// Enabled says whether THIS deployment serves it. A surface listed and disabled is the
	// answer that stops a 404 being the discovery mechanism.
	Enabled bool
	// Note says what the surface is, in one sentence, so a console rendering a name it does
	// not recognise still has something to show a person.
	Note string
}

// Document is the whole self-description: what this deployment serves, who may reach it,
// how a listing may be queried, and what a write accepts.
type Document struct {
	Surfaces []Surface
	Routes   []Route
	Listings []DescribedListing
	// Rules are the conventions every listing shares. They come from internal/table's own
	// values rather than being restated here.
	Rules  Rules
	Bodies []DescribedBody
}

// Route is one served route with what reaching it requires.
type Route struct {
	Method string
	Path   string
	// Access is how the route is reached: public, authenticated, or privileged.
	Access string
	// Permission is what a privileged route requires, and empty for anything else.
	Permission string
}

// DescribedListing is one listing's contract, flattened for a reader.
type DescribedListing struct {
	Method      string
	Path        string
	Searchable  bool
	Sortable    []string
	DefaultSort string
	Filters     []string
}

// DescribedBody is one request body's shape.
type DescribedBody struct {
	Method string
	Path   string
	Schema map[string]any
}

// Rules are the conventions every listing on this surface shares.
type Rules struct {
	// Reserved are the parameter names every listing takes whatever it lists. A listing
	// that does not offer search refuses search rather than ignoring it, which is why these
	// are stated rather than discovered one 400 at a time.
	Reserved []string
	// SortPrefixes are the signs a sort field may carry: minus for descending, plus for
	// ascending.
	SortPrefixes []string
	DefaultLimit int
	MaxLimit     int
	// LimitIsClamped says a limit above the ceiling is reduced rather than refused, because
	// somebody asking for more than they can have wants as much as they can have.
	LimitIsClamped bool
}

// rules reports the conventions, read from internal/table rather than restated. A change to
// the ceiling is a change to this document with no second edit.
func rules() Rules {
	return Rules{
		Reserved:       table.Reserved(),
		SortPrefixes:   table.SortPrefixes(),
		DefaultLimit:   table.DefaultLimit,
		MaxLimit:       table.MaxLimit,
		LimitIsClamped: true,
	}
}

// Describe assembles the document from the route table and what each capability said.
//
// The two arguments come from the SAME handler values, at the one place both are built, so
// a description assembled from a different route table than the one being served is not a
// state this process can reach.
func Describe(routes authz.Table, contributions []Contribution) Document {
	document := Document{Rules: rules(), Surfaces: []Surface{}}

	for _, route := range routes {
		document.Routes = append(document.Routes, Route{
			Method:     route.Method(),
			Path:       route.Pattern(),
			Access:     route.Access().String(),
			Permission: string(route.Permission()),
		})
	}

	for _, contribution := range contributions {
		document.Surfaces = append(document.Surfaces, contribution.Surfaces...)
		for _, listing := range contribution.Listings {
			method, path := split(listing.Route)
			document.Listings = append(document.Listings, DescribedListing{
				Method:      method,
				Path:        path,
				Searchable:  listing.Spec.Searchable,
				Sortable:    listing.Spec.Sortable,
				DefaultSort: listing.Spec.DefaultSort.String(),
				Filters:     filtersOf(listing.Spec),
			})
		}
		for _, body := range contribution.Bodies {
			method, path := split(body.Route)
			document.Bodies = append(document.Bodies, DescribedBody{
				Method: method, Path: path, Schema: SchemaOf(body.Example),
			})
		}
	}

	// Ordered by path and then method throughout, so two builds of the same deployment
	// produce the same document and a diff between two deployments is a real difference
	// rather than map iteration order.
	sort.Slice(document.Routes, func(i, j int) bool {
		return before(document.Routes[i].Path, document.Routes[i].Method,
			document.Routes[j].Path, document.Routes[j].Method)
	})
	sort.Slice(document.Listings, func(i, j int) bool {
		return before(document.Listings[i].Path, document.Listings[i].Method,
			document.Listings[j].Path, document.Listings[j].Method)
	})
	sort.Slice(document.Bodies, func(i, j int) bool {
		return before(document.Bodies[i].Path, document.Bodies[i].Method,
			document.Bodies[j].Path, document.Bodies[j].Method)
	})
	sort.Slice(document.Surfaces, func(i, j int) bool {
		return document.Surfaces[i].Name < document.Surfaces[j].Name
	})
	return document
}

func before(leftPath, leftMethod, rightPath, rightMethod string) bool {
	if leftPath != rightPath {
		return leftPath < rightPath
	}
	return leftMethod < rightMethod
}

// filtersOf answers an empty list rather than nil for a listing that narrows by nothing,
// because "this listing accepts no filters" is a fact worth rendering and a client should
// not have to handle two spellings of it.
func filtersOf(spec table.Spec) []string {
	if spec.Filters == nil {
		return []string{}
	}
	return spec.Filters
}

// split takes a route key apart into the method and the pattern it was built from.
func split(key string) (string, string) {
	for index := range len(key) {
		if key[index] == ' ' {
			return key[:index], key[index+1:]
		}
	}
	return "", key
}
