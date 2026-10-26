package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// ToolName is the stable provider alias of a canonical Tool identity.
func ToolName(name string) string {
	stem, _, qualified := strings.Cut(name, "__")
	alias := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_' || character == '-' {
			return character
		}
		return '_'
	}, stem)
	if !qualified && len(alias) > 0 && len(alias) <= 64 {
		return alias
	}
	if alias == "" {
		alias = "tool"
	}
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%.51s_%x", alias, digest[:6])
}

// ToolNames resolves only the aliases offered by this model request.
func ToolNames(prompt Prompt) (map[string]string, error) {
	names := make(map[string]string, len(prompt.Tools))
	for _, definition := range prompt.Tools {
		alias := ToolName(definition.Name)
		if previous, found := names[alias]; definition.Name == "" || found && previous != definition.Name {
			return nil, fmt.Errorf("offered Tool names are empty or ambiguous")
		}
		names[alias] = definition.Name
	}
	if prompt.ForceTool != "" && names[ToolName(prompt.ForceTool)] != prompt.ForceTool {
		return nil, fmt.Errorf("forced Tool was not offered")
	}
	return names, nil
}
