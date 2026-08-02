package zai

import (
	"encoding/json"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/reasoning"
)

// Turning a provider-neutral prompt into this vendor's request.
//
// The interesting part is what this adapter has to do that the other one does not. This vendor's
// JSON mode returns valid JSON but does not enforce a schema, so the schema is rendered into the
// prompt here — at the very end, after everything cacheable, because a schema appended before the
// cache boundary would move bytes in the prefix.
//
// Doing it here rather than in the shared prompt is the whole point of the capability matrix: the
// workaround for a missing capability belongs to the provider that is missing it, so a vendor that
// enforces schemas is not charged for a vendor that does not.

// request is this vendor's chat completion body.
type request struct {
	Model     string    `json:"model"`
	Messages  []message `json:"messages"`
	MaxTokens int64     `json:"max_tokens"`
	// Stream is explicitly false. The usage figures arrive complete in one body this way, and a
	// cost figure that is silently absent disables the cost ceiling without saying so.
	Stream          bool            `json:"stream"`
	Thinking        *thinking       `json:"thinking,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type thinking struct {
	Type string `json:"type"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// request builds the body for one prompt.
func (p *Provider) request(prompt reasoning.Prompt) request {
	return request{
		Model:     prompt.Model,
		MaxTokens: prompt.MaxOutputTokens,
		Stream:    false,
		Messages: []message{
			{Role: "system", Content: joined(prompt.System)},
			{Role: "user", Content: userContent(prompt)},
		},
		// Thinking is asked for explicitly on this vendor, where the other one has it on by
		// default. Depth is the effort level rather than a token budget.
		Thinking:        &thinking{Type: "enabled"},
		ReasoningEffort: effortOf(prompt.Effort),
		// A JSON mode rather than a schema. It stops the model wrapping the answer in prose; it
		// does not stop the answer being the wrong shape, which is what the decoder is for.
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
}

// userContent renders the deliberation and appends the schema the answer must match.
//
// The schema goes last, after everything the prompt marked cacheable, so the cacheable prefix is
// byte-identical to what it would have been on a provider that enforces schemas natively.
func userContent(prompt reasoning.Prompt) string {
	content := &strings.Builder{}
	content.WriteString(joined(prompt.Content))
	content.WriteString("\n\nReturn one JSON object and nothing else — no prose, no markdown, no " +
		"code fence. It must match this JSON Schema exactly, including every required field:\n\n")
	content.Write(renderedSchema(prompt.Schema))
	return content.String()
}

// renderedSchema writes the schema deterministically. Go's JSON encoder sorts map keys, so the
// same schema renders the same bytes every time — which matters because these bytes are part of
// what a prompt cache would key on.
func renderedSchema(schema reasoning.Schema) []byte {
	encoded, err := json.MarshalIndent(schema.Document, "", "  ")
	if err != nil {
		// The schemas are compiled-in literals, so this cannot happen from configuration. Saying
		// so beats returning a silent empty schema that would make every answer the wrong shape.
		return []byte("{}")
	}
	return encoded
}

// joined concatenates the prompt's blocks. This vendor takes whole messages rather than content
// blocks, so the cache boundaries the blocks carry are not expressible on the wire — the ordering
// they enforce still is, and that is what keeps the prefix stable.
func joined(blocks []reasoning.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

// effortOf maps this system's effort vocabulary onto the vendor's, which offers a finer scale at
// the bottom than this system uses.
func effortOf(effort reasoning.Effort) string {
	switch effort {
	case reasoning.EffortLow:
		return "low"
	case reasoning.EffortMedium:
		return "medium"
	case reasoning.EffortExtraHigh, reasoning.EffortMax:
		return "max"
	default:
		return "high"
	}
}
