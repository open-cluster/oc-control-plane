package controlplane

import (
	"fmt"
	"strings"
)

type evalProfile struct {
	name           string
	runs           int
	requiredPasses int
	caseNames      []string
}

func evalProfileNamed(name string) (evalProfile, error) {
	switch strings.TrimSpace(strings.ToLower(name)) {
	case "", "v0.1-budget":
		return evalProfile{
			name:           "v0.1-budget",
			runs:           1,
			requiredPasses: 1,
			caseNames: []string{
				"single-root-cause",
				"multiple-contributing-causes",
				"conflicting-evidence",
				"missing-data-unresolved",
				"irrelevant-integration-distractors",
				"failed-tool-response",
				"conversation-memory-across-bounded-history",
				"live-hypothesis-updates",
				"postmortem-omissions",
				"peacetime-which-revision-is-deployed",
			},
		}, nil
	case "exhaustive":
		return evalProfile{name: "exhaustive", runs: 3, requiredPasses: 2}, nil
	default:
		return evalProfile{}, fmt.Errorf(
			"OC_EVAL_PROFILE must be exhaustive or v0.1-budget, got %q", name)
	}
}

func evalCasesForProfile(profile evalProfile, available []evalCase) ([]evalCase, error) {
	if profile.caseNames == nil {
		return available, nil
	}
	byName := make(map[string]evalCase, len(available))
	for _, one := range available {
		if _, exists := byName[one.Name]; exists {
			return nil, fmt.Errorf("evaluation fixture %q is duplicated", one.Name)
		}
		byName[one.Name] = one
	}
	selected := make([]evalCase, 0, len(profile.caseNames))
	for _, name := range profile.caseNames {
		one, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("evaluation profile %q needs missing fixture %q",
				profile.name, name)
		}
		selected = append(selected, one)
	}
	return selected, nil
}
