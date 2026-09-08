package agent

import (
	"encoding/json"
	"fmt"
)

type requestSizer interface {
	RequestTokens(Prompt) (int, error)
}

// EstimateSerializedRequest counts every UTF-8 byte as one token, then adds ten percent. A token
// cannot represent less than one byte, so this remains conservative without a vendor tokenizer.
func EstimateSerializedRequest(encoded []byte) int {
	base := len(encoded)
	return base + (base+9)/10
}

// EstimatePromptTokens is the fallback for injected models without a provider encoder.
func EstimatePromptTokens(prompt Prompt) (int, error) {
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return 0, fmt.Errorf("encoding the model request for budgeting: %w", err)
	}
	return EstimateSerializedRequest(encoded), nil
}

func requestTokens(model Model, prompt Prompt) (int, error) {
	if sizer, ok := model.(requestSizer); ok {
		return sizer.RequestTokens(prompt)
	}
	return EstimatePromptTokens(prompt)
}

func (r *Agent) budgetPrompt(prompt Prompt, mayReduceOutput bool) (Prompt, bool, error) {
	input, err := requestTokens(r.model, prompt)
	if err != nil {
		return Prompt{}, false, err
	}
	remaining := int64(r.deployment.ContextWindowTokens - input)
	if remaining >= prompt.MaxOutputTokens {
		return prompt, true, nil
	}
	if !mayReduceOutput || remaining <= 0 {
		return prompt, false, nil
	}
	prompt.MaxOutputTokens = remaining
	input, err = requestTokens(r.model, prompt)
	if err != nil {
		return Prompt{}, false, err
	}
	remaining = int64(r.deployment.ContextWindowTokens - input)
	if remaining <= 0 {
		return prompt, false, nil
	}
	if prompt.MaxOutputTokens > remaining {
		prompt.MaxOutputTokens = remaining
	}
	return prompt, true, nil
}
