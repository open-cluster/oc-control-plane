package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/message"
	"github.com/open-cluster/oc-control-plane/internal/app"
	"github.com/open-cluster/oc-control-plane/internal/config"
)

func TestAssignedMessageBeyondPreviewReachesModel(t *testing.T) {
	address := freeAddress(t)
	prompts := make(chan []*message.Msg, 4)
	const answer = "Use eu-west-2."
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
	}, app.Options{Model: concludingModel{prompts: prompts, factualAnswer: answer}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	request := strings.Repeat("context ", 300) + "Which region should we use? The corrected region is eu-west-2."
	conversationID, turn := plane.openConversation(t, "complete request", request)
	final := plane.awaitInvestigation(t, turn)
	var result struct {
		Status           string `json:"status"`
		ConclusionStatus string `json:"conclusionStatus"`
		Summary          string `json:"summary"`
	}
	decodeInto(t, final, &result)
	if result.Status != "concluded" || result.ConclusionStatus != "" || result.Summary != answer {
		t.Fatalf("expected a visible factual reply without a causal assessment: %s", final)
	}
	select {
	case prompt := <-prompts:
		var rendered strings.Builder
		for _, msg := range prompt {
			if content := msg.GetTextContent("\n"); content != nil {
				rendered.WriteString(*content)
			}
		}
		if !strings.Contains(rendered.String(), request) {
			t.Fatal("the complete assigned Message did not reach the model")
		}
	default:
		t.Fatal("Investigation did not invoke the model")
	}
	const followUp = "Check the same region for errors."
	plane.seedLegacyTurn(t, conversationID, followUp)
	select {
	case prompt := <-prompts:
		var priorRequest, currentRequest bool
		var acceptedAnswers int
		for _, msg := range prompt {
			if text := msg.GetTextContent("\n"); text != nil {
				priorRequest = priorRequest || strings.Contains(*text, request)
				currentRequest = currentRequest || strings.Contains(*text, followUp)
				acceptedAnswers += strings.Count(*text, answer)
				if strings.Contains(*text, "Produce the Investigation Conclusion") {
					t.Fatal("the report instruction leaked into the committed Conversation checkpoint")
				}
				if strings.Contains(*text, "No cause established.") {
					t.Fatal("the exploratory recap duplicated the accepted reply")
				}
			}
		}
		if !priorRequest || !currentRequest || acceptedAnswers != 1 {
			t.Fatalf("follow-up context had prior Message=%t, current Message=%t, accepted answers=%d", priorRequest, currentRequest, acceptedAnswers)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follow-up did not invoke the model")
	}
}

func TestLargeAssignedInputReachesModelIntact(t *testing.T) {
	address := freeAddress(t)
	prompts := make(chan []*message.Msg, 4)
	running := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.HTTPListenAddress = address
		cfg.ModelProvider, cfg.ModelName, cfg.ModelAPIKey = "openrouter", "z-ai/glm-4.7", "scripted-model-key"
		cfg.ModelMaxOutputTokens = 32000
	}, app.Options{Model: concludingModel{prompts: prompts}})
	plane := &integrationPlane{controlPlane: running, api: address, intake: address}
	request := strings.Repeat("界", 8192)
	_, turn := plane.openConversation(t, "large input", request)
	final := plane.awaitInvestigation(t, turn)
	var result struct {
		Status      string `json:"status"`
		Summary     string `json:"summary"`
		Limitations []struct {
			Type             string  `json:"type"`
			MessageSequences []int64 `json:"messageSequences"`
		} `json:"limitations"`
	}
	if err := json.Unmarshal([]byte(final), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "concluded" || len(result.Limitations) != 0 {
		t.Fatalf("a valid request was treated as a capacity failure or missing input: %s", final)
	}
	select {
	case prompt := <-prompts:
		if input := prompt[len(prompt)-1].GetTextContent("\n"); input == nil || !strings.Contains(*input, request) {
			t.Fatal("the complete large Message did not reach the model")
		}
	default:
		t.Fatal("the request was refused without calling the model")
	}
}
