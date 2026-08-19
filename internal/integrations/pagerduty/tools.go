package pagerduty

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxIncidentsPerList     = 100
	defaultIncidentsPerList = 25
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one PagerDuty API call per invocation, against the account's " +
	"shared rate budget; a handful of calls per investigation is fine, a scan is not"

// validStatuses and validUrgencies are the vendor's own closed vocabularies, checked here
// so a typo is refused with a plain message rather than reaching the vendor as a silently
// empty filter.
var (
	validStatuses  = []string{"triggered", "acknowledged", "resolved"}
	validUrgencies = []string{"high", "low"}
)

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listIncidentsTool(client),
		getIncidentTool(client),
	}
}

// incidentContent is one incident as a tool reports it.
type incidentContent struct {
	ID          string `json:"id"`
	Number      int    `json:"number,omitempty"`
	Title       string `json:"title"`
	Status      string `json:"status,omitempty"`
	Urgency     string `json:"urgency,omitempty"`
	CreatedAt   string `json:"createdAt,omitempty"`
	ServiceName string `json:"serviceName,omitempty"`
	URL         string `json:"url,omitempty"`
}

func listIncidentsTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "status",
			Description: "Comma separated statuses to filter by: " +
				strings.Join(validStatuses, ", ") + ".",
			Type: integrations.FieldString,
		},
		{
			Name: "urgency",
			Description: "Comma separated urgencies to filter by: " +
				strings.Join(validUrgencies, ", ") + ".",
			Type: integrations.FieldString,
		},
		{
			Name:        "since",
			Description: "Start of the date range, ISO 8601. Defaults to one month back.",
			Type:        integrations.FieldString,
		},
		{
			Name:        "until",
			Description: "End of the date range, ISO 8601.",
			Type:        integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many incidents to return, at most %d. Default %d.",
				maxIncidentsPerList, defaultIncidentsPerList),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "pagerduty.list_incidents",
		Capability: ListIncidents,
		Description: "Lists the account's incidents, filtered by status, urgency or a " +
			"date range, newest first.",
		WhenToUse: "First, to find which incidents are open for a service during the " +
			"incident's own window: filter by status or urgency.",
		WhenNotToUse: "Not for one incident's full detail — that is " +
			"pagerduty.get_incident. Not repeatedly with rephrasings of one filter.",
		Arguments:   declared,
		Permissions: "the token needs the incidents.read scope",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of incidents, each with id, number, title, status, " +
			"urgency, service and creation time, plus a truncated flag when the " +
			"account holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultIncidentsPerList, maxIncidentsPerList)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			statuses, err := values.enumList("status", validStatuses)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			urgencies, err := values.enumList("urgency", validUrgencies)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			since, err := values.text("since")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			until, err := values.text("until")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Incidents(ctx, request.Credential, IncidentsQuery{
				Statuses: statuses, Urgencies: urgencies, Since: since, Until: until, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}

			content := make([]incidentContent, 0, len(listed.Incidents))
			sources := make([]string, 0, len(listed.Incidents))
			for _, incident := range listed.Incidents {
				content = append(content, incidentToContent(incident))
				sources = append(sources, incident.ID)
			}
			return integrations.ToolResult{
				Content:   content,
				Truncated: listed.Truncated,
				Summary:   fmt.Sprintf("%d incidents matched", len(content)),
				Sources:   sources,
			}, nil
		},
	}
}

func getIncidentTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "incidentId",
			Description: "The incident id from pagerduty.list_incidents.",
			Type:        integrations.FieldString,
			Required:    true,
		},
	}
	return integrations.Tool{
		Name:        "pagerduty.get_incident",
		Capability:  GetIncident,
		Description: "Reads one incident whole: its status, urgency, service and timing.",
		WhenToUse: "After pagerduty.list_incidents has selected one incident, to read its " +
			"full detail before citing it as evidence.",
		WhenNotToUse: "Not before an incident is selected — choose it with " +
			"pagerduty.list_incidents first. Not speculatively across every incident returned.",
		Arguments:   declared,
		Permissions: "the token needs the incidents.read scope",
		RateLimit:   rateLimitNote,
		Output:      "one incident, with id, number, title, status, urgency, service and creation time",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			incidentID, err := values.required("incidentId")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			incident, err := client.Incident(ctx, request.Credential, incidentID)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content: incidentToContent(incident),
				Summary: incident.Title + " (" + incident.Status + ")",
				Sources: []string{incident.ID},
			}, nil
		},
	}
}

func incidentToContent(incident Incident) incidentContent {
	return incidentContent{
		ID: incident.ID, Number: incident.Number, Title: incident.Title, Status: incident.Status,
		Urgency: incident.Urgency, CreatedAt: incident.CreatedAt,
		ServiceName: incident.ServiceName, URL: incident.HTMLURL,
	}
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
		term = strings.ToLower(strings.TrimSpace(term))
		if !allowedSet[term] {
			return nil, fmt.Errorf("%s %q is not one of %s", name, term, strings.Join(allowed, ", "))
		}
		terms = append(terms, term)
	}
	return terms, nil
}
