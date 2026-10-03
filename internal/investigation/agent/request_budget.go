package agent

import (
	"encoding/json"
	"fmt"
)

type requestSizer interface {
	RequestTokens(Prompt) (int, error)
}

func EstimateSerializedRequest(encoded []byte) int {
	base := len(encoded)
	return base + (base+9)/10
}

func EstimatePromptTokens(prompt Prompt) (int, error) {
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return 0, fmt.Errorf("encoding the model request for budgeting: %w", err)
	}
	return EstimateSerializedRequest(encoded), nil
}

func requestTokens(completer Completer, prompt Prompt) (int, error) {
	if sizer, ok := completer.(requestSizer); ok {
		return sizer.RequestTokens(prompt)
	}
	return EstimatePromptTokens(prompt)
}

func (r *Agent) budgetPrompt(prompt Prompt, mayReduceOutput bool) (Prompt, bool, error) {
	input, err := requestTokens(r.completer, prompt)
	if err != nil {
		return Prompt{}, false, err
	}
	remaining := int64(r.modelConfig.ContextWindowTokens - input)
	if remaining >= prompt.MaxOutputTokens {
		return prompt, true, nil
	}
	if !mayReduceOutput || remaining <= 0 {
		return prompt, false, nil
	}
	prompt.MaxOutputTokens = remaining
	input, err = requestTokens(r.completer, prompt)
	if err != nil {
		return Prompt{}, false, err
	}
	remaining = int64(r.modelConfig.ContextWindowTokens - input)
	if remaining <= 0 {
		return prompt, false, nil
	}
	if prompt.MaxOutputTokens > remaining {
		prompt.MaxOutputTokens = remaining
	}
	return prompt, true, nil
}
