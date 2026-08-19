package newrelic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxIssuesPerList     = 100
	defaultIssuesPerList = 25
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one NerdGraph call per invocation, against the account's shared " +
	"rate budget; a handful of calls per investigation is fine, a scan is not"

// validPriorities and validStates are New Relic's own closed vocabularies, checked here so
// a typo is refused with a plain message rather than reaching the vendor as a silently
// empty filter.
var (
	validPriorities = []string{"LOW", "MEDIUM", "HIGH", "CRITICAL"}
	validStates     = []string{"ACTIVATED", "ACKNOWLEDGED", "CLOSED"}
)

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listIssuesTool(client),
		getIssueTool(client),
	}
}

// issueContent is one issue as a tool reports it.
type issueContent struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Priority       string   `json:"priority,omitempty"`
	State          string   `json:"state,omitempty"`
	EntityNames    []string `json:"entityNames,omitempty"`
	Description    string   `json:"description,omitempty"`
	ActivatedAt    int64    `json:"activatedAt,omitempty"`
	AcknowledgedAt int64    `json:"acknowledgedAt,omitempty"`
	ClosedAt       int64    `json:"closedAt,omitempty"`
}

func listIssuesTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "priority",
			Description: "Comma separated priorities to filter by: " +
				strings.Join(validPriorities, ", ") + ".",
			Type: integrations.FieldString,
		},
		{
			Name: "state",
			Description: "Comma separated states to filter by: " +
				strings.Join(validStates, ", ") + ".",
			Type: integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many issues to return, at most %d. Default %d.",
				maxIssuesPerList, defaultIssuesPerList),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "newrelic.list_issues",
		Capability: ListIssues,
		Description: "Lists the account's correlated issues, filtered by priority or " +
			"state, most recently activated first.",
		WhenToUse: "First, to find which issues are active for a service during the " +
			"incident's window: filter by priority or narrow to ACTIVATED.",
		WhenNotToUse: "Not for one issue's full detail — that is newrelic.get_issue. Not " +
			"repeatedly with rephrasings of one filter.",
		Arguments:   declared,
		Permissions: "the user key must be granted read access to this account",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of issues, each with id, title, priority, state, the " +
			"entities involved and activation time, plus a truncated flag when the " +
			"account holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			region, accountID, err := regionAndAccountOf(request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultIssuesPerList, maxIssuesPerList)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			priorities, err := values.enumList("priority", validPriorities)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			states, err := values.enumList("state", validStates)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Issues(ctx, region, request.Credential, accountID,
				IssuesQuery{Priorities: priorities, States: states, Limit: limit})
			if err != nil {
				return integrations.ToolResult{}, err
			}

			content := make([]issueContent, 0, len(listed.Issues))
			sources := make([]string, 0, len(listed.Issues))
			for _, issue := range listed.Issues {
				content = append(content, issueContent(issue))
				sources = append(sources, issue.ID)
			}
			return integrations.ToolResult{
				Content:   content,
				Truncated: listed.Truncated,
				Summary:   fmt.Sprintf("%d issues matched", len(content)),
				Sources:   sources,
			}, nil
		},
	}
}

func getIssueTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "issueId",
			Description: "The issue id from newrelic.list_issues.",
			Type:        integrations.FieldString,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name:        "newrelic.get_issue",
		Capability:  GetIssue,
		Description: "Reads one issue whole: its priority, state, entities and description.",
		WhenToUse: "After newrelic.list_issues has selected one issue, to read its full " +
			"detail before citing it as evidence.",
		WhenNotToUse: "Not before an issue is selected — choose it with " +
			"newrelic.list_issues first. Not speculatively across every issue returned.",
		Arguments:   declared,
		Permissions: "the user key must be granted read access to this account",
		RateLimit:   rateLimitNote,
		Output:      "one issue, with id, title, priority, state, entities and description",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			region, accountID, err := regionAndAccountOf(request.Integration)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			issueID, err := values.required("issueId")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			issue, err := client.Issue(ctx, region, request.Credential, accountID, issueID)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content: issueContent(issue),
				Summary: issue.Title + " (" + issue.Priority + ")",
				Sources: []string{issue.ID},
			}, nil
		},
	}
}

// regionAndAccountOf reads the region and account id an Integration is configured with.
func regionAndAccountOf(integration integrations.Integration) (string, int, error) {
	region, isText := integration.Configuration["region"].(string)
	if !isText || origins[region] == "" {
		return "", 0, errors.New("region is not set to one this provider knows on this integration")
	}
	accountID, err := accountIDOf(integration.Configuration["accountId"])
	if err != nil {
		return "", 0, err
	}
	return region, accountID, nil
}

// arguments is one call's inputs after the undeclared ones were refused.
type arguments struct {
	values map[string]any
}

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

func (a arguments) required(name string) (string, error) {
	text, err := a.text(name)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", errors.New(name + " is required")
	}
	return text, nil
}

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

// enumList reads a comma separated argument, refusing any term outside the closed set.
func (a arguments) enumList(name string, allowed []string) ([]string, error) {
	text, err := a.text(name)
	if err != nil || text == "" {
		return nil, err
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, one := range allowed {
		allowedSet[one] = true
	}
	var terms []string
	for _, term := range strings.Split(text, ",") {
		term = strings.ToUpper(strings.TrimSpace(term))
		if !allowedSet[term] {
			return nil, fmt.Errorf("%s %q is not one of %s", name, term, strings.Join(allowed, ", "))
		}
		terms = append(terms, term)
	}
	return terms, nil
}
