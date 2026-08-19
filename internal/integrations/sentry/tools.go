package sentry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The two read-only tools. Every bound is named rather than implied, and truncation is
// flagged from the vendor's own pagination.

// The named bounds. The maximum follows the vendor's own page ceiling; the default is
// sized for an investigation reading context, not for export.
const (
	maxIssuesPerList     = 100
	defaultIssuesPerList = 25
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one Sentry API call per invocation, against the organization's " +
	"shared rate budget; a handful of calls per investigation is fine, a scan is not"

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listIssuesTool(client),
		getIssueTool(client),
	}
}

// issueContent is one issue as a tool reports it.
type issueContent struct {
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit,omitempty"`
	Level     string `json:"level,omitempty"`
	Status    string `json:"status,omitempty"`
	Count     string `json:"count,omitempty"`
	UserCount string `json:"userCount,omitempty"`
	FirstSeen string `json:"firstSeen,omitempty"`
	LastSeen  string `json:"lastSeen,omitempty"`
	Permalink string `json:"permalink,omitempty"`
	Project   string `json:"project,omitempty"`
}

func listIssuesTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "query",
			Description: "Sentry's own search syntax, such as \"is:unresolved\", " +
				"\"level:error\", or a service name. Empty returns the organization's " +
				"default unresolved view.",
			Type: integrations.FieldString,
		},
		{
			Name: "statsPeriod",
			Description: "The relative window, such as \"24h\" or \"14d\". Use the " +
				"incident's own window when one is known.",
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
		Name:       "sentry.list_issues",
		Capability: ListIssues,
		Description: "Lists an organization's issues, filtered by Sentry's own search " +
			"syntax, newest activity first.",
		WhenToUse: "First, to find which issues are active for a service during the " +
			"incident's window: filter by level, status, or a service name in the query.",
		WhenNotToUse: "Not for one issue's full detail, tags, or event context — that is " +
			"sentry.get_issue. Not repeatedly with rephrasings of one query.",
		Arguments:   declared,
		Permissions: "the auth token needs the org:read and project:read scopes",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of issues, each with id, shortId, title, culprit, level, " +
			"status, count, userCount and permalink, plus a truncated flag when the " +
			"organization holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			organizationSlug, err := organizationSlugOf(request.Integration)
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
			query, err := values.text("query")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			statsPeriod, err := values.text("statsPeriod")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Issues(ctx, request.Credential, organizationSlug, IssuesQuery{
				Query: query, StatsPeriod: statsPeriod, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}

			content := make([]issueContent, 0, len(listed.Issues))
			sources := make([]string, 0, len(listed.Issues))
			for _, issue := range listed.Issues {
				content = append(content, issueToContent(issue))
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
			Description: "The issue id from sentry.list_issues.",
			Type:        integrations.FieldString,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name:        "sentry.get_issue",
		Capability:  GetIssue,
		Description: "Reads one issue whole: its status, counts, culprit and permalink.",
		WhenToUse: "After sentry.list_issues has selected one issue, to read its full " +
			"detail before citing it as evidence.",
		WhenNotToUse: "Not before an issue is selected — choose it with " +
			"sentry.list_issues first. Not speculatively across every issue returned.",
		Arguments:   declared,
		Permissions: "the auth token needs the event:read scope",
		RateLimit:   rateLimitNote,
		Output:      "one issue, with id, shortId, title, culprit, level, status, count, userCount and permalink",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			organizationSlug, err := organizationSlugOf(request.Integration)
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

			issue, err := client.Issue(ctx, request.Credential, organizationSlug, issueID)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content: issueToContent(issue),
				Summary: issue.ShortID + ": " + issue.Title,
				Sources: []string{issue.ID},
			}, nil
		},
	}
}

// organizationSlugOf reads the organization slug an Integration is configured with.
func organizationSlugOf(integration integrations.Integration) (string, error) {
	slug, isText := integration.Configuration["organizationSlug"].(string)
	if !isText || strings.TrimSpace(slug) == "" {
		return "", errors.New("organizationSlug is not set on this integration")
	}
	return slug, nil
}

func issueToContent(issue Issue) issueContent {
	content := issueContent{
		ID: issue.ID, ShortID: issue.ShortID, Title: issue.Title, Culprit: issue.Culprit,
		Level: issue.Level, Status: issue.Status, Count: issue.Count,
		UserCount: issue.UserCount, Permalink: issue.Permalink, Project: issue.Project,
	}
	if !issue.FirstSeen.IsZero() {
		content.FirstSeen = issue.FirstSeen.Format(time.RFC3339)
	}
	if !issue.LastSeen.IsZero() {
		content.LastSeen = issue.LastSeen.Format(time.RFC3339)
	}
	return content
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

// required reads a string argument that must be present and non-empty.
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

// count reads a bounded whole number, applying the default when absent.
func (a arguments) count(name string, fallback, maximum int) (int, error) {
	value, given := a.values[name]
	if !given {
		return fallback, nil
	}
	// JSON numbers arrive as float64; a whole number is required, not merely truncated.
	number, isNumber := value.(float64)
	if !isNumber || number != float64(int64(number)) || number < 1 || int(number) > maximum {
		return 0, fmt.Errorf("%s must be a whole number between 1 and %d", name, maximum)
	}
	return int(number), nil
}
