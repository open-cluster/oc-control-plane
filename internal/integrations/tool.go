package integrations

import "context"

// Tool is one bounded read a provider offers an investigation, declared beside the
// capability it exercises.
//
// The metadata is not documentation garnish: it is what anything routing between tools
// reads, so a tool that cannot say when NOT to use it is a tool that gets used wrongly
// and then patched with prompts. Every field except the argument list is required at
// catalog assembly.
type Tool struct {
	// Name identifies the tool: "slack.get_channel_history". Stable, because provenance
	// records carry it.
	Name string
	// Capability is the declared capability this tool exercises. It must be one the
	// definition declares, which is checked at catalog assembly.
	Capability string
	// Description says what the tool does, in one or two sentences.
	Description string
	// WhenToUse says which questions this tool answers.
	WhenToUse string
	// WhenNotToUse names the misuses: the questions a caller will be tempted to bring
	// here that belong to another tool or nowhere.
	WhenNotToUse string
	// Arguments is what a call may carry. An argument nothing declares is refused by the
	// tool, never dropped.
	Arguments []ToolArgument
	// Permissions names what the connected account must be granted for this tool to work.
	Permissions string
	// RateLimit says what calling this costs against the provider's limits, so a caller
	// can weigh a call rather than discover the ceiling.
	RateLimit string
	// Output says what a successful answer holds.
	Output string
	// Run performs the read, bounded by the declared arguments and the request's context.
	Run func(ctx context.Context, request ToolRequest) (ToolResult, error)
}

// ToolArgument is one declared input.
type ToolArgument struct {
	Name        string
	Description string
	Type        FieldType
	Required    bool
}

// ToolRequest is one call. The credential is the unsealed outbound credential, present
// only for the duration of the call and never stored by anything downstream of it.
type ToolRequest struct {
	Integration Integration
	Credential  string
	Arguments   map[string]any
}

// ToolResult is one answer.
type ToolResult struct {
	// Content is the answer in types that marshal to JSON; its shape is the tool's Output
	// promise kept.
	Content any
	// Truncated reports that the source held more than the bound, so a reader cannot
	// mistake a page for the whole.
	Truncated bool
}
