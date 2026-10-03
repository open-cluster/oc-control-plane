package slack

import (
	"strings"
	"testing"
)

func TestNoToolPromisesAnUnboundedRecentTail(t *testing.T) {
	t.Parallel()

	for _, tool := range tools(nil) {
		text := tool.Description + " " + tool.WhenToUse + " " + tool.WhenNotToUse
		for _, argument := range tool.Arguments {
			text += " " + argument.Description
		}
		if strings.Contains(strings.ToLower(text), "recent tail") {
			t.Errorf("%s promises a recent tail; every windowed read is clamped into "+
				"the investigation's window and there is no unbounded path", tool.Name)
		}
	}
}
