package investigation

import (
	"sort"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The context router: a small deterministic scorer, not an LLM and not a knowledge
// graph. It decides WHICH connected sources an investigation reads, so that a handful of
// relevant integrations are queried instead of everything an organization connected —
// and so that the selection can be explained afterwards, because every choice records
// the terms that made it.

// Router bounds. The selection is deliberately small: an investigation that reads
// everything is slow, expensive and no more likely to be right.
const (
	// selectedSources is how many sources the first round reads.
	selectedSources = 2
	// expansionSources is how many more may join when the first selection returned
	// nothing at all.
	expansionSources = 1
)

// selection is one candidate the router chose, with the reason stated.
type selection struct {
	integration integrations.Integration
	tools       []integrations.Tool
	score       int
	reason      string
}

// route ranks the organization's candidates against the subject and returns the initial
// selection and the ranked remainder the expansion may draw from.
//
// Candidates are integrations whose type declares tools — the ones that can actually be
// read. Ranking is term overlap: the subject's own words (episode title, labels, the
// operator's question) against each integration's name and labels, with a small weight
// for a source whose type reads conversation when the subject carries incident language.
// Deterministic on purpose: the same alert routes the same way twice, and the reason is
// a sentence an operator can check.
func route(
	catalog integrations.Catalog, candidates []integrations.Integration, terms []string,
) (selected, remainder []selection) {
	scored := make([]selection, 0, len(candidates))
	for _, candidate := range candidates {
		definition, known := catalog.ByID(candidate.Type)
		if !known || candidate.Disabled() {
			continue
		}
		tools := offeredTools(definition, candidate)
		if len(tools) == 0 {
			continue
		}
		score, matched := overlap(candidate, terms)
		scored = append(scored, selection{
			integration: candidate,
			tools:       tools,
			score:       score,
			reason:      reasonFor(definition, matched),
		})
	}

	// Ties break on name so the routing is stable run to run; a selection that depended
	// on map order would make the same alert route differently twice.
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].integration.Name < scored[j].integration.Name
	})

	cut := selectedSources
	if cut > len(scored) {
		cut = len(scored)
	}
	return scored[:cut], scored[cut:]
}

// offeredTools filters a definition's tools to those this integration's verified
// grants support. Fail closed: a tool whose Requires are not all recorded is absent
// from the investigation's set, never a call that always fails — which is how a pasted
// bot token stops being offered user-token-only search.
func offeredTools(
	definition integrations.Definition, candidate integrations.Integration,
) []integrations.Tool {
	recorded := make(map[string]bool, len(candidate.VerifyGrants))
	for _, grant := range candidate.VerifyGrants {
		recorded[grant] = true
	}
	offered := make([]integrations.Tool, 0, len(definition.Tools))
	for _, tool := range definition.Tools {
		supported := true
		for _, required := range tool.Requires {
			if !recorded[required] {
				supported = false
				break
			}
		}
		if supported {
			offered = append(offered, tool)
		}
	}
	return offered
}

// overlap scores one candidate: how many subject terms appear in its name or labels.
func overlap(candidate integrations.Integration, terms []string) (int, []string) {
	haystack := strings.ToLower(candidate.Name)
	for key, value := range candidate.Labels {
		haystack += " " + strings.ToLower(key) + " " + strings.ToLower(value)
	}

	score := 0
	var matched []string
	for _, term := range terms {
		if strings.Contains(haystack, term) {
			score++
			matched = append(matched, term)
		}
	}
	sort.Strings(matched)
	return score, matched
}

// reasonFor writes the selection's explanation, in the operator's language.
func reasonFor(definition integrations.Definition, matched []string) string {
	if len(matched) == 0 {
		return "a connected " + definition.Name + " source; nothing in its name or labels " +
			"matched the subject"
	}
	return "a connected " + definition.Name + " source matching " +
		strings.Join(matched, ", ")
}

// subjectTerms reduces the subject and question to the words worth matching: lower-cased,
// punctuation split, short noise words dropped. Deliberately dumb — a stemmer or an
// embedding here would make the routing unexplainable, and the explanation is the point.
func subjectTerms(parts ...string) []string {
	seen := map[string]bool{}
	var terms []string
	for _, part := range parts {
		for _, word := range strings.FieldsFunc(strings.ToLower(part), func(r rune) bool {
			letter := 'a' <= r && r <= 'z'
			digit := '0' <= r && r <= '9'
			return !letter && !digit && r != '-' && r != '_'
		}) {
			if len(word) < 3 || noiseWords[word] || seen[word] {
				continue
			}
			seen[word] = true
			terms = append(terms, word)
		}
	}
	return terms
}

// noiseWords are the words that match everything and select nothing.
var noiseWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "what": true, "why": true,
	"how": true, "is": true, "are": true, "was": true, "has": true, "have": true,
	"our": true, "this": true, "that": true, "not": true, "you": true, "about": true,
	"happening": true, "wrong": true, "down": true, "broken": true,
}
