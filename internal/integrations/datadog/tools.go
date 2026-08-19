package datadog

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The two read-only tools. Every bound is named rather than implied, and a page that came
// back full is flagged: the v1 monitors listing states no total, so a full page is the
// only signal available that more may exist.

const (
	maxMonitorsPerList     = 100
	defaultMonitorsPerList = 25
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one Datadog API call per invocation, against the account's shared " +
	"rate budget; a handful of calls per investigation is fine, a scan is not"

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listMonitorsTool(client),
		getMonitorTool(client),
	}
}

// monitorContent is one monitor as a tool reports it.
type monitorContent struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type,omitempty"`
	Query        string   `json:"query,omitempty"`
	Message      string   `json:"message,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	OverallState string   `json:"overallState,omitempty"`
	Created      string   `json:"created,omitempty"`
	Modified     string   `json:"modified,omitempty"`
}

func listMonitorsTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "name",
			Description: "A substring of the monitor's name, such as a service name.",
			Type:        integrations.FieldString,
		},
		{
			Name: "tags",
			Description: "A comma separated list of monitor tags, Datadog's own filter " +
				"syntax, such as \"service:checkout,env:prod\".",
			Type: integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many monitors to return, at most %d. Default %d.",
				maxMonitorsPerList, defaultMonitorsPerList),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "datadog.list_monitors",
		Capability: ListMonitors,
		Description: "Lists the account's monitors, filtered by name or tags, with their " +
			"current alert state.",
		WhenToUse: "First, to find which monitors are alerting for a service during the " +
			"incident's window: filter by a service or environment tag.",
		WhenNotToUse: "Not for one monitor's full detail — that is datadog.get_monitor. " +
			"Not repeatedly with rephrasings of one filter.",
		Arguments:   declared,
		Permissions: "the account's api key and application key must both be valid",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of monitors, each with id, name, type, query, message, " +
			"tags and overall state, plus a truncated flag when the account holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			site, cred, err := siteAndCredentialOf(request)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultMonitorsPerList, maxMonitorsPerList)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			name, err := values.text("name")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			tags, err := values.text("tags")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Monitors(ctx, site, cred, MonitorsQuery{Name: name, Tags: tags, Limit: limit})
			if err != nil {
				return integrations.ToolResult{}, err
			}

			content := make([]monitorContent, 0, len(listed.Monitors))
			sources := make([]string, 0, len(listed.Monitors))
			for _, monitor := range listed.Monitors {
				content = append(content, monitorToContent(monitor))
				sources = append(sources, strconv.FormatInt(monitor.ID, 10))
			}
			return integrations.ToolResult{
				Content:   content,
				Truncated: listed.Truncated,
				Summary:   fmt.Sprintf("%d monitors matched", len(content)),
				Sources:   sources,
			}, nil
		},
	}
}

func getMonitorTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "monitorId",
			Description: "The monitor id from datadog.list_monitors.",
			Type:        integrations.FieldInteger,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name:        "datadog.get_monitor",
		Capability:  GetMonitor,
		Description: "Reads one monitor whole: its query, message, tags and current state.",
		WhenToUse: "After datadog.list_monitors has selected one monitor, to read its full " +
			"detail before citing it as evidence.",
		WhenNotToUse: "Not before a monitor is selected — choose it with " +
			"datadog.list_monitors first. Not speculatively across every monitor returned.",
		Arguments:   declared,
		Permissions: "the account's api key and application key must both be valid",
		RateLimit:   rateLimitNote,
		Output:      "one monitor, with id, name, type, query, message, tags and overall state",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			site, cred, err := siteAndCredentialOf(request)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			id, err := values.wholeNumber("monitorId")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			monitor, err := client.Monitor(ctx, site, cred, id)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content: monitorToContent(monitor),
				Summary: monitor.Name + ": " + monitor.OverallState,
				Sources: []string{strconv.FormatInt(monitor.ID, 10)},
			}, nil
		},
	}
}

// siteAndCredentialOf reads the site an Integration is configured with and unseals its
// credential pair. Both travel from the request: the site is non-secret configuration,
// the credential is the unsealed outbound secret present only for this call.
func siteAndCredentialOf(request integrations.ToolRequest) (string, credential, error) {
	site, isText := request.Integration.Configuration["site"].(string)
	if !isText || strings.TrimSpace(site) == "" {
		return "", credential{}, errors.New("site is not set on this integration")
	}
	cred, err := parseCredential(request.Credential)
	if err != nil {
		return "", credential{}, err
	}
	return site, cred, nil
}

func monitorToContent(monitor Monitor) monitorContent {
	return monitorContent{
		ID: monitor.ID, Name: monitor.Name, Type: monitor.Type, Query: monitor.Query,
		Message: monitor.Message, Tags: monitor.Tags, OverallState: monitor.OverallState,
		Created: monitor.Created, Modified: monitor.Modified,
	}
}

// arguments is one call's inputs after the undeclared ones were refused.
type arguments struct {
	values map[string]any
}

// readArguments refuses an argument nothing declares. Dropped arguments are the quiet
// failure mode of tool calling: the caller believes it narrowed the read and it did not.
func readArguments(
	declared []integrations.ToolArgument, given map[string]any,
) (arguments, error) {
	for name := range given {
		if !declaresArgument(declared, name) {
			return arguments{}, fmt.Errorf("argument %q is not one this tool declares", name)
		}
	}
	return arguments{values: given}, nil
}

func declaresArgument(declared []integrations.ToolArgument, name string) bool {
	for _, argument := range declared {
		if argument.Name == name {
			return true
		}
	}
	return false
}

// text reads an optional string argument.
func (a arguments) text(name string) (string, error) {
	value, given := a.values[name]
	if !given {
		return "", nil
	}
	text, isText := value.(string)
	if !isText {
		return "", fmt.Errorf("%s must be text", name)
	}
	return strings.TrimSpace(text), nil
}

// count reads a bounded whole number, applying the default when absent.
func (a arguments) count(name string, fallback, maximum int) (int, error) {
	value, given := a.values[name]
	if !given {
		return fallback, nil
	}
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 || int(number) > maximum {
		return 0, fmt.Errorf("%s must be a whole number between 1 and %d", name, maximum)
	}
	return int(number), nil
}

// wholeNumber reads a required whole number with no upper bound of its own — an id rather
// than a count.
func (a arguments) wholeNumber(name string) (int64, error) {
	value, given := a.values[name]
	if !given {
		return 0, errors.New(name + " is required")
	}
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) {
		return 0, fmt.Errorf("%s must be a whole number", name)
	}
	return int64(number), nil
}
