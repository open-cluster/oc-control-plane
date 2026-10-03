package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

type Secret string

const redacted = "[redacted]"

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return `"` + redacted + `"` }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) LogValue() any                { return redacted }
func (s Secret) Reveal() string             { return string(s) }
func (s Secret) Empty() bool                { return strings.TrimSpace(string(s)) == "" }

type ModelConfig struct {
	Provider            string
	Model               string
	Effort              Effort
	ContextWindowTokens int
	MaxOutputTokens     int64
	BaseURL             string
	Credential          Secret
	RequestTimeout      time.Duration
}

const (
	defaultRequestTimeout = 5 * time.Minute
)

func (d ModelConfig) WithDefaults() ModelConfig {
	if d.Effort == "" {
		d.Effort = EffortHigh
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = defaultRequestTimeout
	}
	return d
}

func (d ModelConfig) Validate() error {
	switch {
	case strings.TrimSpace(d.Provider) == "":
		return fmt.Errorf("model configuration must name a provider")
	case strings.TrimSpace(d.Model) == "":
		return fmt.Errorf("the %s provider must name an exact model identifier", d.Provider)
	case !d.Effort.Valid():
		return fmt.Errorf("the %s provider names effort %q, which is not one of low, medium, "+
			"high, xhigh or max", d.Provider, d.Effort)
	case d.Credential.Empty():
		return fmt.Errorf("the %s provider has no credential; it is read from a file path so "+
			"that it cannot leak through a process listing", d.Provider)
	}
	if d.BaseURL != "" {
		parsed, err := url.Parse(d.BaseURL)
		loopback := false
		if err == nil {
			address := net.ParseIP(parsed.Hostname())
			loopback = parsed.Hostname() == "localhost" || address != nil && address.IsLoopback()
		}
		if err != nil || parsed.Host == "" ||
			(parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopback)) {
			return fmt.Errorf(
				"the %s provider names a base url that is not an https host or local "+
					"loopback; the adapter may reach that host and nothing else", d.Provider)
		}
	}
	return nil
}

func (d ModelConfig) String() string {
	return fmt.Sprintf("%s/%s effort=%s context=%d max_output=%d", d.Provider, d.Model, d.Effort,
		d.ContextWindowTokens, d.MaxOutputTokens)
}

type ModelCapabilities struct {
	ContextWindowTokens int
	MaxOutputTokens     int64
}

func ResolveModelCapabilities(
	config ModelConfig, published *ModelCapabilities,
) (ModelConfig, error) {
	if published == nil {
		if config.ContextWindowTokens <= 0 || config.MaxOutputTokens <= 0 {
			return ModelConfig{}, fmt.Errorf("the custom model %q requires explicit context and output limits",
				config.Model)
		}
	} else {
		if config.ContextWindowTokens <= 0 {
			config.ContextWindowTokens = published.ContextWindowTokens
		} else if config.ContextWindowTokens > published.ContextWindowTokens {
			return ModelConfig{}, fmt.Errorf("the context limit %d exceeds model %q's published limit %d",
				config.ContextWindowTokens, config.Model, published.ContextWindowTokens)
		}
		if config.MaxOutputTokens <= 0 {
			config.MaxOutputTokens = published.MaxOutputTokens
		} else if config.MaxOutputTokens > published.MaxOutputTokens {
			return ModelConfig{}, fmt.Errorf("the output limit %d exceeds model %q's published limit %d",
				config.MaxOutputTokens, config.Model, published.MaxOutputTokens)
		}
	}
	if int64(config.ContextWindowTokens) <= config.MaxOutputTokens {
		return ModelConfig{}, fmt.Errorf("model %q's context limit must exceed its output limit", config.Model)
	}
	return config, nil
}

type Completer interface {
	Complete(ctx context.Context, prompt Prompt) (Completion, error)
}

type Prompt struct {
	Model           string
	System          []Block
	Content         []Block
	Schema          Schema
	Tools           []integrations.ToolDefinition
	ForceTool       string
	Turns           []Turn
	MaxOutputTokens int64
	Effort          Effort
}

type Turn struct {
	Assistant   AssistantTurn
	Results     []ToolResultTurn
	Instruction string
}

type AssistantTurn struct {
	Text  string
	Calls []CompletionCall
	Raw   []byte
}

type CompletionCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ToolResultTurn struct {
	CallID  string
	Content string
	IsError bool
}

type Block struct {
	Text  string
	Cache bool
}

type Schema struct {
	Name     string
	Version  string
	Document map[string]any
}

type Completion struct {
	Model     string
	RequestID string
	Document  []byte
	ToolCalls []CompletionCall
	Raw       []byte
	Stop      Stop
	Usage     TokenUsage
}

type Stop int16

const (
	StopComplete Stop = iota + 1
	StopRefused
	StopTruncated
	StopToolUse
)

func (s Stop) String() string {
	switch s {
	case StopComplete:
		return "complete"
	case StopRefused:
		return "refused"
	case StopTruncated:
		return "truncated"
	case StopToolUse:
		return "tool_use"
	default:
		return "unrecognised"
	}
}

type Effort string

const (
	EffortLow       Effort = "low"
	EffortMedium    Effort = "medium"
	EffortHigh      Effort = "high"
	EffortExtraHigh Effort = "xhigh"
	EffortMax       Effort = "max"
)

func (e Effort) Valid() bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortExtraHigh, EffortMax:
		return true
	default:
		return false
	}
}

type Count struct {
	Tokens   int64
	Reported bool
}

func Counted(tokens int64) Count { return Count{Tokens: tokens, Reported: true} }

func Unreported() Count { return Count{} }

func (c Count) Or(fallback int64) int64 {
	if !c.Reported {
		return fallback
	}
	return c.Tokens
}

type TokenUsage struct {
	Input      Count
	Output     Count
	CacheWrite Count
	CacheRead  Count
	Reasoning  Count
}

func usageOf(usage TokenUsage) investigation.Usage {
	return investigation.Usage{
		InputTokens:  usage.Input.Or(0) + usage.CacheRead.Or(0) + usage.CacheWrite.Or(0),
		OutputTokens: usage.Output.Or(0),
	}
}
