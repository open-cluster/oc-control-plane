package audit

import (
	"sort"
	"strings"
)

// credentialWords are the substrings that make a key's value unfit for the record.
//
// The list is deliberately broad. A false positive costs an auditor one field of context; a
// false negative writes a live credential into a table the database will not let anyone delete
// from, on every database, forever.
var credentialWords = []string{
	"secret", "token", "password", "passwd", "credential",
	"apikey", "api_key", "privatekey", "private_key", "authorization", "cookie",
	"digest", "signature", "assertion",
}

// Detail is the structured context one event carries — the previous and new value of a changed
// setting, the reason a request was refused, the count something acted on.
//
// It never holds a credential and never holds raw source content. That is enforced by Safe
// rather than by a rule call sites are asked to remember, because the paths that write these
// events are precisely the paths holding a secret at the time.
type Detail map[string]any

// Safe returns the detail as it may be stored: keys naming a credential dropped, values
// bounded, and the whole map bounded. It is called on the way into storage, so nothing that
// builds an event has to remember to call it.
//
// A nil detail stays nil. Most events carry no structure and should not gain an empty object.
func (d Detail) Safe() Detail {
	if len(d) == 0 {
		return nil
	}

	// Sorted, so that dropping the overflow keeps the same entries every time. An arbitrary
	// map order would make one write keep a field and the next drop it, which is the kind of
	// difference that reads as tampering months later.
	keys := make([]string, 0, len(d))
	for key := range d {
		if !namesACredential(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > MaxDetailEntries {
		keys = keys[:MaxDetailEntries]
	}

	safe := make(Detail, len(keys))
	for _, key := range keys {
		safe[key] = boundedValue(d[key])
	}
	if len(safe) == 0 {
		return nil
	}
	return safe
}

// NamesACredential reports whether a key's value is unfit to leave the control plane.
//
// It is exported so that the other paths carrying structured data outward — the
// investigation event stream — drop the same keys by the same rule. One list of forbidden
// words is the whole point: a second copy is a second place for one of them to be missing,
// and the one that goes missing is the one that matters.
func NamesACredential(key string) bool { return namesACredential(key) }

func namesACredential(key string) bool {
	folded := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
	for _, word := range credentialWords {
		if strings.Contains(folded, strings.ReplaceAll(word, "_", "")) {
			return true
		}
	}
	return false
}

// boundedValue cuts a string to the limit and leaves anything else alone. Numbers, booleans
// and small lists are what the rest of the detail is made of, and none of them can be inflated
// by a caller the way a string can.
func boundedValue(value any) any {
	text, isText := value.(string)
	if !isText {
		return value
	}
	return truncate(text, MaxDetailValueLength)
}
