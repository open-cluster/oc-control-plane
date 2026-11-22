package ai

import _ "embed"

//go:embed prompts/investigation.md
var InvestigationInstructions string

//go:embed prompts/conclusion.md
var ConclusionInstructions string
