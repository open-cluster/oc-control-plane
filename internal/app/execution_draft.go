package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-cluster/oc-control-plane/internal/auth/authz"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	storage "github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

const draftFlushInterval = 250 * time.Millisecond

type executionDraft struct {
	persist     func(context.Context, string) error
	secretForms []string
	text        []rune
	savedText   *string // nil when a write's outcome is unknown and must be retried
	nextFlush   time.Time
	timer       *time.Timer
}

func (d *executionDraft) startModelCall(ctx context.Context) {
	d.stopTimer()
	d.text = d.text[:0]
	d.flush(ctx)
}

func (d *executionDraft) appendText(ctx context.Context, delta string) {
	if d.persist == nil || len(d.text) == conversation.MaxMessageTextLength {
		return
	}
	for _, r := range delta {
		if r != 0 {
			d.text = append(d.text, r)
			if len(d.text) == conversation.MaxMessageTextLength {
				break
			}
		}
	}
	if d.timer != nil {
		return
	}
	if wait := time.Until(d.nextFlush); wait > 0 {
		d.timer = time.NewTimer(wait)
	} else {
		d.flush(ctx)
	}
}

func (d *executionDraft) flushReady() <-chan time.Time {
	if d.timer != nil {
		return d.timer.C
	}
	return nil
}

func (d *executionDraft) stopTimer() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func (d *executionDraft) flush(ctx context.Context) {
	if d.persist == nil {
		return
	}
	text := conversation.Bounded(safeDraftText(string(d.text), d.secretForms), conversation.MaxMessageTextLength)
	if d.savedText != nil && text == *d.savedText {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := d.persist(writeCtx, text); err != nil {
		// An error can leave the result of a commit unknown.
		d.savedText = nil
		if errors.Is(err, storage.ErrAgentLeaseLost) || errors.Is(err, authz.ErrNotAMember) ||
			errors.Is(err, storage.ErrAgentToolRunUnavailable) {
			d.persist = nil
		} else if ctx.Err() == nil && d.timer == nil {
			d.timer = time.NewTimer(draftFlushInterval)
		}
		return
	}
	d.savedText = &text
	d.nextFlush = time.Now().Add(draftFlushInterval)
	if text == "" {
		d.nextFlush = time.Time{}
	}
}

func safeDraftText(raw string, secrets []string) string {
	// A trailing proper prefix may complete into a secret in the next delta.
	raw = redactModelSecret(raw, secrets)
	withhold := 0
	for _, value := range secrets {
		for length := min(len(value)-1, len(raw)); length > withhold; length-- {
			if strings.HasSuffix(raw, value[:length]) {
				withhold = length
				break
			}
		}
	}
	return raw[:len(raw)-withhold]
}

func redactModelSecret(text string, secrets []string) string {
	for _, value := range secrets {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}

func modelSecretForms(secret string) []string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil
	}
	quoted, _ := json.Marshal(secret)
	escaped := string(quoted[1 : len(quoted)-1])
	if escaped == secret {
		return []string{secret}
	}
	return []string{escaped, secret}
}
