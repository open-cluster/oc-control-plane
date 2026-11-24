package conversation

import (
	"context"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (h Handlers) rename(writer http.ResponseWriter, request *http.Request) {
	principal, org, id, ok := h.addressed(writer, request)
	if !ok {
		return
	}
	var input struct {
		Title string `json:"title"`
	}
	if !h.decode(writer, request, &input) {
		return
	}
	title := strings.TrimSpace(input.Title)
	if title == "" || utf8.RuneCountInString(title) > MaxTitleLength {
		writeJSON(writer, http.StatusBadRequest, errorView{Error: "give a title of at most 512 characters"})
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), readTimeout)
	defer cancel()
	renamed, err := h.Store.RenameConversation(ctx, principal, org, id, title)
	if err != nil {
		h.fail(writer, request, err)
		return
	}
	writeJSON(writer, http.StatusOK, conversationViewOf(renamed))
}

func CleanAutomaticTitle(text string) string {
	text = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r), r == '<', r == '>':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, text)
	text = strings.Trim(strings.Join(strings.Fields(text), " "), " \"'`#*_")
	return strings.TrimSpace(boundedRunes(text, 72))
}
