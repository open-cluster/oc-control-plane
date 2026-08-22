package slack

import (
	"strings"
	"testing"
)

// The contract must not promise a read the implementation cannot perform. Channel history
// told the model that an unbounded read returns the channel's recent tail; every read is
// clamped into the investigation's own window, so it never does. The mirror of this guard
// lives in the github package, because the same false promise was made twice.
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
