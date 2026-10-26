package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// ToolNames translates provider aliases without changing canonical Tool identity.
type ToolNames struct {
	aliases map[string]string
	offered map[string]string
}

// ProviderToolNames retains the Investigation's naming context during finalization.
func (p Prompt) ProviderToolNames() (ToolNames, error) {
	bases := make(map[string]string)
	counts := make(map[string]int)
	add := func(name string) {
		if _, exists := bases[name]; exists {
			return
		}
		base := strings.Map(func(char rune) rune {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
				char >= '0' && char <= '9' || char == '_' || char == '-' {
				return char
			}
			return '_'
		}, name)
		bases[name] = base
		counts[base]++
	}
	for _, definition := range p.availableTools {
		add(definition.Name)
	}
	for _, definition := range p.Tools {
		add(definition.Name)
	}
	for _, turn := range p.Turns {
		for _, call := range turn.Assistant.Calls {
			add(call.Name)
		}
	}
	names := ToolNames{aliases: make(map[string]string), offered: make(map[string]string)}
	used := make(map[string]string)
	for canonical, base := range bases {
		if canonical == "" {
			return ToolNames{}, fmt.Errorf("tool name is empty")
		}
		alias := base
		if len(base) > 64 || counts[base] > 1 {
			digest := sha256.Sum256([]byte(canonical))
			if len(base) > 51 {
				base = base[:51]
			}
			alias = fmt.Sprintf("%s_%x", base, digest[:6])
		}
		if previous, exists := used[alias]; exists && previous != canonical {
			return ToolNames{}, fmt.Errorf("tool aliases are ambiguous")
		}
		used[alias] = canonical
		names.aliases[canonical] = alias
	}
	for _, definition := range p.Tools {
		names.offered[names.Alias(definition.Name)] = definition.Name
	}
	if p.ForceTool != "" {
		if _, offered := names.Resolve(names.Alias(p.ForceTool)); !offered {
			return ToolNames{}, fmt.Errorf("forced Tool is not offered")
		}
	}
	return names, nil
}

// Alias returns the provider name for a known canonical Tool.
func (n ToolNames) Alias(canonical string) string { return n.aliases[canonical] }

// Resolve accepts only aliases offered in the current request.
func (n ToolNames) Resolve(alias string) (string, bool) {
	canonical, offered := n.offered[alias]
	return canonical, offered
}
