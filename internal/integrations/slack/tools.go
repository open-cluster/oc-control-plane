package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

const (
	maxChannelsPerList     = 200
	defaultChannelsPerList = 100
	maxChannelPages        = 5
	maxMessagesPerRead     = 200
	defaultMessagesPerRead = 100
	maxThreadReplies       = 200
	maxSearchMatches       = 100
	defaultSearchMatches   = 20
)

func tools(client *Client) []integrations.Tool {
	return []integrations.Tool{
		listChannelsTool(client),
		channelHistoryTool(client),
		threadRepliesTool(client),
		searchMessagesTool(client),
	}
}

type channelContent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Members int    `json:"members"`
}

type messageContent struct {
	TS         string `json:"ts"`
	User       string `json:"user,omitempty"`
	UserID     string `json:"userId,omitempty"`
	Text       string `json:"text"`
	ThreadTS   string `json:"threadTs,omitempty"`
	ReplyCount int    `json:"replyCount,omitempty"`
	Channel    string `json:"channel,omitempty"`
	ChannelID  string `json:"channelId,omitempty"`
	Permalink  string `json:"permalink,omitempty"`
}

func renderMessages(
	ctx context.Context, client *Client, request integrations.ToolRequest,
	channel string, messages []Message,
) []messageContent {
	workspace := ""
	if len(messages) > 0 {
		workspace = client.WorkspaceURL(ctx, request.Credential)
	}
	content := make([]messageContent, 0, len(messages))
	for _, one := range messages {
		rendered := messageContent{
			TS: one.TS, Text: one.Text, ThreadTS: one.ThreadTS,
			ReplyCount: one.ReplyCount, Channel: one.Channel, ChannelID: one.ChannelID,
		}
		if one.User != "" {
			rendered.User = one.User
			if looksLikeUserID(one.User) {
				rendered.UserID = one.User
				if name := client.UserName(ctx, request.Credential, one.User); name != "" {
					rendered.User = name
				}
			}
		}
		at := channel
		if at == "" {
			at = one.ChannelID
		}
		if workspace != "" && at != "" {
			rendered.Permalink = permalink(workspace, at, one.TS)
		}
		content = append(content, rendered)
	}
	return content
}

func looksLikeUserID(user string) bool {
	if len(user) < 8 || (user[0] != 'U' && user[0] != 'W') {
		return false
	}
	for _, r := range user {
		upper := 'A' <= r && r <= 'Z'
		digit := '0' <= r && r <= '9'
		if !upper && !digit {
			return false
		}
	}
	return true
}

func permalink(workspace, channel, ts string) string {
	return strings.TrimSuffix(workspace, "/") + "/archives/" + channel +
		"/p" + strings.ReplaceAll(ts, ".", "")
}

func Permalink(workspace, channel, ts string) string { return permalink(workspace, channel, ts) }

func listChannelsTool(client *Client) integrations.Tool {
	declared := []integrations.ToolArgument{
		{
			Name: "nameContains",
			Description: "Optional case-insensitive text to select channels by name, " +
				"topic or purpose. Omit it to list every channel: one unfiltered " +
				"listing shows what exists, where guessed filter terms miss channels " +
				"whose names you did not predict. Filter only when an unfiltered " +
				"listing came back truncated.",
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
		Name: "slack.list_channels",
		Description: "List public, unarchived Slack channels with IDs, names, topics, " +
			"purposes, and member counts. Use to select relevant channels before reading " +
			"history. Do not use to read messages or search for where a phrase was said. " +
			"Results are bounded and report truncation.",
		Arguments:      declared,
		RequiredGrants: []string{"channels:read"},
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultChannelsPerList, maxChannelsPerList)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			needle, err := values.Text("nameContains")
			if err != nil {
				return integrations.ToolResult{}, err
			}

			var selected []channelContent
			var sources []string
			matched, cursor := 0, ""
			for range maxChannelPages {
				listed, err := client.Channels(
					ctx, request.Credential, maxChannelsPerList, cursor)
				if err != nil {
					return integrations.ToolResult{}, err
				}
				for _, channel := range listed.Channels {
					if !matchesChannel(channel, needle) {
						continue
					}
					matched++
					if len(selected) < limit {
						selected = append(selected, channelContent(channel))
						sources = append(sources, channel.ID)
					}
				}
				cursor = listed.NextCursor
				if cursor == "" || len(selected) >= limit {
					break
				}
			}
			return integrations.ToolResult{
				Content:   selected,
				Truncated: matched > len(selected) || cursor != "",
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
				"omitting the window does NOT widen the read: every read is clamped into the " +
				"investigation's own window, and the result states what it covered.",
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
		Name: "slack.get_channel_history",
		Description: "Read messages from one known Slack channel in the Investigation " +
			"window, including authors, thread markers, reply counts, and links. Use for " +
			"incident discussion, reactions, and reported actions. Do not use for thread " +
			"replies or workspace-wide search. Slack discussion is testimony and requires " +
			"corroboration. Results are bounded and report truncation.",
		Arguments:      declared,
		RequiredGrants: []string{"channels:history"},
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			channel, err := values.Required("channel")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultMessagesPerRead, maxMessagesPerRead)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			oldest, err := values.Moment("oldest")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			latest, err := values.Moment("latest")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			oldest, latest = request.ClampWindow(oldest, latest)

			read, err := client.History(ctx, request.Credential, HistoryQuery{
				Channel: channel, Oldest: oldest, Latest: latest, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content:    renderMessages(ctx, client, request, channel, read.Messages),
				Truncated:  read.Truncated,
				Summary:    fmt.Sprintf("%d messages in %s", len(read.Messages), channel),
				Sources:    []string{channel},
				WindowFrom: oldest, WindowUntil: latest,
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
		Name: "slack.get_thread_replies",
		Description: "Read the newest bounded tail of a known Slack thread in message " +
			"order, including authors, text, and links. Use after channel history identifies " +
			"a message with replies. Do not use for a channel timeline or speculatively on " +
			"messages without replies. Slack discussion is testimony and requires " +
			"corroboration. Results report truncation.",
		Arguments:           declared,
		RequiredGrants:      []string{"channels:history"},
		SupportsThreadScope: true,
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			channel, err := values.Required("channel")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			thread, err := values.Required("threadTs")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			if request.OriginChannel != "" || request.OriginThread != "" {
				if channel != request.OriginChannel || thread != request.OriginThread {
					return integrations.ToolResult{}, errors.New(
						"the read is outside the originating conversation thread")
				}
			}
			limit, err := values.Count("limit", defaultMessagesPerRead, maxThreadReplies)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			tail, err := client.Replies(ctx, request.Credential, RepliesQuery{
				Channel: channel, ThreadTS: thread, Limit: limit,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			summary := fmt.Sprintf("%d thread messages", len(tail.Messages))
			if tail.Walked > len(tail.Messages) {
				summary = fmt.Sprintf("the newest %d of %d thread messages",
					len(tail.Messages), tail.Walked)
			}
			if !tail.WalkEnded {
				summary += "; the thread continues past what was walked"
			}
			return integrations.ToolResult{
				Content:   renderMessages(ctx, client, request, channel, tail.Messages),
				Truncated: tail.Walked > len(tail.Messages) || !tail.WalkEnded,
				Summary:   summary,
				Sources:   []string{channel + "/" + thread},
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
		Name: "slack.search_messages",
		Description: "Search Slack messages for exact identifiers across the workspace " +
			"within the Investigation window. Use when the relevant channel is unknown and " +
			"an error string, ticket, service, or hostname is available. Do not use vague " +
			"prose or substitute search results for reading the selected channel. Slack " +
			"discussion is testimony and requires corroboration. Results are bounded and " +
			"report truncation.",
		Arguments:      declared,
		RequiredGrants: []string{"search:read", grantUserToken},
		Run: func(ctx context.Context, request integrations.ToolRequest) (integrations.ToolResult, error) {
			values, err := integrations.ReadArguments(declared, request.Arguments)
			if err != nil {
				return integrations.ToolResult{}, err
			}
			query, err := values.Required("query")
			if err != nil {
				return integrations.ToolResult{}, err
			}
			limit, err := values.Count("limit", defaultSearchMatches, maxSearchMatches)
			if err != nil {
				return integrations.ToolResult{}, err
			}

			found, err := client.Search(ctx, request.Credential, SearchQuery{
				Query: query, Count: limit,
				After: request.WindowFrom, Before: request.WindowUntil,
			})
			if err != nil {
				return integrations.ToolResult{}, err
			}
			return integrations.ToolResult{
				Content:   renderMessages(ctx, client, request, "", found.Matches),
				Truncated: found.Truncated,
				Summary:   fmt.Sprintf("%d matches", len(found.Matches)),
				Sources:   matchChannels(found.Matches),
			}, nil
		},
	}
}

func matchChannels(matches []Message) []string {
	seen := map[string]bool{}
	var channels []string
	for _, match := range matches {
		name := match.ChannelID
		if name == "" {
			name = match.Channel
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		channels = append(channels, name)
	}
	return channels
}

func matchesChannel(channel Channel, needle string) bool {
	if needle == "" {
		return true
	}
	needle = strings.ToLower(needle)
	return strings.Contains(strings.ToLower(channel.Name), needle) ||
		strings.Contains(strings.ToLower(channel.Topic), needle) ||
		strings.Contains(strings.ToLower(channel.Purpose), needle)
}
