package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-cluster/oc-control-plane/internal/integrations"
)

// The four read-only tools. Every bound here is named rather than implied, every
// truncation is flagged honestly: a long thread is read from its newest end with the
// flag saying so, message authors are resolved to names, and every message carries a
// permalink an operator can follow.

// The named bounds. The maxima follow the vendor's own page ceilings; the defaults are
// sized for an investigation reading context, not for export.
const (
	maxChannelsPerList     = 200
	defaultChannelsPerList = 100
	// maxChannelPages bounds the listing walk. Five vendor pages of 200 cover any
	// workspace an investigation plausibly reads; past that the result flags truncation
	// rather than scanning on.
	maxChannelPages        = 5
	maxMessagesPerRead     = 200
	defaultMessagesPerRead = 100
	maxThreadReplies       = 200
	maxSearchMatches       = 100
	defaultSearchMatches   = 20
)

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

// messageContent is one message as a tool reports it: the author as a human-readable
// name beside the raw id, and a permalink an operator can follow to the message itself.
type messageContent struct {
	TS string `json:"ts"`
	// User is the resolved display name, or the raw id when the workspace will not
	// resolve it; UserID keeps the raw id either way.
	User       string `json:"user,omitempty"`
	UserID     string `json:"userId,omitempty"`
	Text       string `json:"text"`
	ThreadTS   string `json:"threadTs,omitempty"`
	ReplyCount int    `json:"replyCount,omitempty"`
	Channel    string `json:"channel,omitempty"`
	ChannelID  string `json:"channelId,omitempty"`
	Permalink  string `json:"permalink,omitempty"`
}

// renderMessages resolves authors and attaches permalinks for one read's messages.
// channel is the channel the read addressed; search matches carry their own channel id
// instead. Resolution is best-effort: a transcript with raw ids beats a failed read.
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

// looksLikeUserID reports whether the author field is a raw id worth resolving; search
// matches already carry names, and resolving a name as an id would waste a call.
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

// permalink builds the workspace's documented archive link for one message — the
// scope-free pointer an operator follows from a finding to the conversation itself.
func permalink(workspace, channel, ts string) string {
	return strings.TrimSuffix(workspace, "/") + "/archives/" + channel +
		"/p" + strings.ReplaceAll(ts, ".", "")
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
			"and purposes, walking the listing far enough that a filter match beyond the " +
			"first page is still found.",
		WhenToUse: "First, to select the few channels worth reading: match the incident's " +
			"service, team or alert names against channel names and topics.",
		WhenNotToUse: "Not for reading messages — it returns no message content. Not for " +
			"finding where something was SAID; that is slack.search_messages. Never as a " +
			"way to enumerate the workspace for its own sake.",
		Arguments:   declared,
		Permissions: "the bot token needs the channels:read scope",
		Requires:    []string{"channels:read"},
		Output: "a bounded list of channels, each with id, name, topic, purpose and member " +
			"count, plus a truncated flag when more matched than were returned or the " +
			"walk stopped before the workspace's end",
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

			// The filter runs inside a bounded pagination walk. Filtering one page
			// client-side was the defect: a matching channel beyond page one was
			// invisible while the answer looked complete.
			var selected []channelContent
			var sources []string
			matched, cursor := 0, ""
			for page := 0; page < maxChannelPages; page++ {
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
		Arguments: declared,
		Permissions: "the bot token needs the channels:history scope; users:read resolves " +
			"authors to names, and without it messages carry raw ids",
		Requires: []string{"channels:history"},
		Output: "a bounded list of messages, each with ts, the author resolved to a " +
			"display name beside the raw id, text, thread marker, reply count and a " +
			"permalink, plus a truncated flag when the window holds more",
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
				Content:   renderMessages(ctx, client, request, channel, read.Messages),
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
		Description: "Reads one thread's messages in order, answering the newest tail of " +
			"a bounded walk with the truncated flag set when the thread held more — the " +
			"end of a war-room thread is where the conclusion lives.",
		WhenToUse: "When channel history showed a message with replies and the discussion " +
			"under it is what matters — triage threads, decision threads.",
		WhenNotToUse: "Not for a channel's timeline; that is slack.get_channel_history. " +
			"Not speculatively on every message — only where the reply count says a " +
			"discussion happened.",
		Arguments: declared,
		Permissions: "the bot token needs the channels:history scope; users:read resolves " +
			"authors to names",
		Requires: []string{"channels:history"},
		Output: "the thread's messages in order — the newest tail of what a bounded walk " +
			"reached — each with ts, the author resolved to a display name, text and a " +
			"permalink; the truncated flag reports a thread longer than what came back, " +
			"and the summary says how much was walked and whether the thread continues",
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
		Name:       "slack.search_messages",
		Capability: SearchMessages,
		Description: "Searches messages across the workspace for exact terms, bounded to " +
			"the investigation's window, with the remainder flagged.",
		WhenToUse: "When the right channel is unknown and an identifier is: an error " +
			"message, a ticket number, a hostname. The matches carry channel ids, so " +
			"the pivot to slack.get_channel_history needs no re-listing.",
		WhenNotToUse: "Not as a substitute for reading a selected channel's window. Not " +
			"with vague prose — the vendor matches terms, not meaning. Not repeatedly " +
			"with rephrasings of one question.",
		Arguments: declared,
		Permissions: "classic message search works only with a user token granted the " +
			"search:read scope; a bot token is never offered this tool",
		Requires: []string{"search:read", GrantUserToken},
		Output: "a bounded list of matches, each with ts, author, text, the channel name " +
			"and id it was said in and a permalink, plus a truncated flag when more " +
			"matched",
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

			// The window is the investigation's own, structurally: the model asks with
			// terms, never with dates.
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

// matchChannels reports the distinct channels the matches were said in, by id where
// the vendor gave one — the id is what a history pivot takes.
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
