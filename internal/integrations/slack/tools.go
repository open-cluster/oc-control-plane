package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The four read-only tools. Every bound here is named rather than implied, every
// truncation is flagged from the vendor's own answer, and a thread longer than its bound
// is refused rather than returned short — part of a thread reads exactly like the whole
// of one, which is how an investigation quotes a conversation that continued past what it
// saw.

// The named bounds. The maxima follow the vendor's own page ceilings; the defaults are
// sized for an investigation reading context, not for export.
const (
	maxChannelsPerList     = 200
	defaultChannelsPerList = 100
	maxMessagesPerRead     = 200
	defaultMessagesPerRead = 100
	maxThreadReplies       = 200
	maxSearchMatches       = 100
	defaultSearchMatches   = 20
)

// rateLimitNote is the same fact on every tool, stated once.
const rateLimitNote = "one Slack Web API call per invocation, against the workspace's shared rate budget; a handful of calls per investigation is fine, a scan is not"

// tools is the declared set, one-to-one with the capabilities the definition declares.
func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listChannelsTool(client),
		channelHistoryTool(client),
		threadRepliesTool(client),
		searchMessagesTool(client),
	}
}

// channelContent is one channel as a tool reports it.
type channelContent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Members int    `json:"members"`
}

// messageContent is one message as a tool reports it.
type messageContent struct {
	TS         string `json:"ts"`
	User       string `json:"user,omitempty"`
	Text       string `json:"text"`
	ThreadTS   string `json:"threadTs,omitempty"`
	ReplyCount int    `json:"replyCount,omitempty"`
	Channel    string `json:"channel,omitempty"`
}

func listChannelsTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "nameContains",
			Description: "Case-insensitive text to select channels by name, topic or " +
				"purpose. Use terms from the incident: a service name, a team name, " +
				"\"incident\", \"alerts\".",
			Type: integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many channels to return, at most %d. Default %d.",
				maxChannelsPerList, defaultChannelsPerList),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "slack.list_channels",
		Capability: ListChannels,
		Description: "Lists the workspace's public, unarchived channels with their topics " +
			"and purposes, so candidate channels can be chosen by name and topic.",
		WhenToUse: "First, to select the few channels worth reading: match the incident's " +
			"service, team or alert names against channel names and topics.",
		WhenNotToUse: "Not for reading messages — it returns no message content. Not for " +
			"finding where something was SAID; that is slack.search_messages. Never as a " +
			"way to enumerate the workspace for its own sake.",
		Arguments:   declared,
		Permissions: "the bot token needs the channels:read scope",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of channels, each with id, name, topic, purpose and member " +
			"count, plus a truncated flag when the workspace holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultChannelsPerList, maxChannelsPerList)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			needle, err := values.text("nameContains")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			listed, err := client.Channels(ctx, request.Credential, limit)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			selected := make([]channelContent, 0, len(listed.Channels))
			sources := make([]string, 0, len(listed.Channels))
			for _, channel := range listed.Channels {
				if !matchesChannel(channel, needle) {
					continue
				}
				selected = append(selected, channelContent(channel))
				sources = append(sources, channel.ID)
			}
			return integrations.ToolResult{
				Content:   selected,
				Truncated: listed.Truncated,
				Summary:   fmt.Sprintf("%d channels matched", len(selected)),
				Sources:   sources,
			}, nil
		},
	}
}

func channelHistoryTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "channel",
			Description: "The channel id from slack.list_channels, such as \"C0123456789\".",
			Type:        integrations.FieldString,
			Required:    true,
		},
		{
			Name: "oldest",
			Description: "Start of the window, RFC 3339. Use the incident's own window; " +
				"an unbounded read returns the channel's recent tail instead.",
			Type: integrations.FieldString,
		},
		{
			Name:        "latest",
			Description: "End of the window, RFC 3339.",
			Type:        integrations.FieldString,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many messages to return, at most %d. Default %d.",
				maxMessagesPerRead, defaultMessagesPerRead),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "slack.get_channel_history",
		Capability: ReadChannelHistory,
		Description: "Reads one channel's messages inside a time window, bounded and " +
			"flagged when the window holds more.",
		WhenToUse: "To read what people said in a selected channel during the incident's " +
			"window: deploy chatter, alarm reactions, operator actions.",
		WhenNotToUse: "Not before channels are selected — choose them with " +
			"slack.list_channels first. Not for a thread's replies; that is " +
			"slack.get_thread_replies. Not across the whole workspace; that is " +
			"slack.search_messages.",
		Arguments:   declared,
		Permissions: "the bot token needs the channels:history scope",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of messages, each with ts, user, text, thread marker and " +
			"reply count, plus a truncated flag when the window holds more",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			channel, err := values.required("channel")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultMessagesPerRead, maxMessagesPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			oldest, err := values.moment("oldest")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			latest, err := values.moment("latest")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			oldest, latest = clampWindow(oldest, latest, request)

			read, err := client.History(ctx, request.Credential, HistoryQuery{
				Channel: channel, Oldest: oldest, Latest: latest, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content:   messagesContent(read.Messages),
				Truncated: read.Truncated,
				Summary:   fmt.Sprintf("%d messages in %s", len(read.Messages), channel),
				Sources:   []string{channel},
			}, nil
		},
	}
}

func threadRepliesTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name:        "channel",
			Description: "The channel id the thread lives in.",
			Type:        integrations.FieldString,
			Required:    true,
		},
		{
			Name: "threadTs",
			Description: "The thread's own ts, taken from a message whose replyCount was " +
				"greater than zero.",
			Type:     integrations.FieldString,
			Required: true,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many replies to return, at most %d. Default %d.",
				maxThreadReplies, defaultMessagesPerRead),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "slack.get_thread_replies",
		Capability: ReadThreads,
		Description: "Reads one thread whole, or refuses. A thread longer than the bound " +
			"is an error rather than a shortened transcript.",
		WhenToUse: "When channel history showed a message with replies and the discussion " +
			"under it is what matters — triage threads, decision threads.",
		WhenNotToUse: "Not for a channel's timeline; that is slack.get_channel_history. " +
			"Not speculatively on every message — only where the reply count says a " +
			"discussion happened.",
		Arguments:   declared,
		Permissions: "the bot token needs the channels:history scope",
		RateLimit:   rateLimitNote,
		Output: "the thread's messages in order, each with ts, user and text; an error " +
			"names the bound when the thread exceeds it",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			channel, err := values.required("channel")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			thread, err := values.required("threadTs")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultMessagesPerRead, maxThreadReplies)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			read, err := client.Replies(ctx, request.Credential, RepliesQuery{
				Channel: channel, ThreadTS: thread, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			if read.Truncated {
				// Refused rather than shortened: part of a thread reads exactly like the
				// whole of one, and a conclusion quoted from a conversation that continued
				// past what was read is the mislead this bound exists to prevent.
				return integrations.ToolResult{}, fmt.Errorf(
					"the thread holds more than %d replies; read the channel window with "+
						"slack.get_channel_history instead of quoting part of a discussion",
					limit)
			}
			return integrations.ToolResult{
				Content: messagesContent(read.Messages),
				Summary: fmt.Sprintf("%d thread messages", len(read.Messages)),
				Sources: []string{channel + "/" + thread},
			}, nil
		},
	}
}

func searchMessagesTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "query",
			Description: "The search terms. Use identifiers from the incident — an error " +
				"string, a service name, a hostname — not prose questions.",
			Type:     integrations.FieldString,
			Required: true,
		},
		{
			Name: "limit",
			Description: fmt.Sprintf("How many matches to return, at most %d. Default %d.",
				maxSearchMatches, defaultSearchMatches),
			Type: integrations.FieldInteger,
		},
	}
	return integrations.Tool{
		Name:       "slack.search_messages",
		Capability: SearchMessages,
		Description: "Searches messages across the workspace for exact terms, bounded, " +
			"with the remainder flagged.",
		WhenToUse: "When the right channel is unknown and an identifier is: an error " +
			"message, a ticket number, a hostname. The matches say where to read next.",
		WhenNotToUse: "Not as a substitute for reading a selected channel's window. Not " +
			"with vague prose — the vendor matches terms, not meaning. Not repeatedly " +
			"with rephrasings of one question.",
		Arguments:   declared,
		Permissions: "the bot token needs the search:read scope",
		RateLimit:   rateLimitNote,
		Output: "a bounded list of matches, each with ts, user, text and the channel it " +
			"was said in, plus a truncated flag when more matched",
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := readArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			query, err := values.required("query")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.count("limit", defaultSearchMatches, maxSearchMatches)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			found, err := client.Search(ctx, request.Credential, SearchQuery{
				Query: query, Count: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content:   messagesContent(found.Matches),
				Truncated: found.Truncated,
				Summary:   fmt.Sprintf("%d matches", len(found.Matches)),
				Sources:   matchChannels(found.Matches),
			}, nil
		},
	}
}

// matchChannels reports the distinct channels the matches were said in.
func matchChannels(matches []Message) []string {
	seen := map[string]bool{}
	var channels []string
	for _, match := range matches {
		if match.Channel == "" || seen[match.Channel] {
			continue
		}
		seen[match.Channel] = true
		channels = append(channels, match.Channel)
	}
	return channels
}

// clampWindow narrows an argument window into the request's own. A read phrased with a
// wider window still runs inside the investigation's: the bound is structural, not
// something a prompt asked for.
func clampWindow(oldest, latest time.Time, request integrations.ToolRequest) (time.Time, time.Time) {
	if !request.WindowFrom.IsZero() && (oldest.IsZero() || oldest.Before(request.WindowFrom)) {
		oldest = request.WindowFrom
	}
	if !request.WindowUntil.IsZero() && (latest.IsZero() || latest.After(request.WindowUntil)) {
		latest = request.WindowUntil
	}
	return oldest, latest
}

// matchesChannel reports whether a channel's name, topic or purpose carries the needle.
// An empty needle selects everything, which is the unfiltered listing.
func matchesChannel(channel Channel, needle string) bool {
	if needle == "" {
		return true
	}
	needle = strings.ToLower(needle)
	return strings.Contains(strings.ToLower(channel.Name), needle) ||
		strings.Contains(strings.ToLower(channel.Topic), needle) ||
		strings.Contains(strings.ToLower(channel.Purpose), needle)
}

func messagesContent(messages []Message) []messageContent {
	content := make([]messageContent, 0, len(messages))
	for _, one := range messages {
		content = append(content, messageContent(one))
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

// moment reads an optional RFC 3339 timestamp.
func (a arguments) moment(name string) (time.Time, error) {
	text, err := a.text(name)
	if err != nil || text == "" {
		return time.Time{}, err
	}
	at, parseErr := time.Parse(time.RFC3339, text)
	if parseErr != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 time such as "+
			"2026-01-02T15:04:05Z", name)
	}
	return at, nil
}
