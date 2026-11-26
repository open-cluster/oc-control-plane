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
	save      func(context.Context, string) error
	secrets   []string
	raw       []rune
	published *string
	nextWrite time.Time
	timer     *time.Timer
}

func (d *executionDraft) reset(ctx context.Context) {
	d.stopTimer()
	d.raw = d.raw[:0]
	d.flush(ctx)
}

func (d *executionDraft) append(ctx context.Context, delta string) {
	if d.save == nil || len(d.raw) == conversation.MaxMessageTextLength {
		return
	}
	for _, r := range delta {
		if r != 0 {
			d.raw = append(d.raw, r)
			if len(d.raw) == conversation.MaxMessageTextLength {
				break
			}
		}
	}
	if d.timer != nil {
		return
	}
	if wait := time.Until(d.nextWrite); wait > 0 {
		d.timer = time.NewTimer(wait)
	} else {
		d.flush(ctx)
	}
}

func (d *executionDraft) ready() <-chan time.Time {
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
	if d.save == nil {
		return
	}
	text := conversation.Bounded(safeDraftText(string(d.raw), d.secrets), conversation.MaxMessageTextLength)
	if d.published != nil && text == *d.published {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := d.save(writeCtx, text); err != nil {
		// An error can leave the result of a commit unknown.
		d.published = nil
		if errors.Is(err, storage.ErrAgentLeaseLost) || errors.Is(err, authz.ErrNotAMember) ||
			errors.Is(err, storage.ErrAgentToolRunUnavailable) {
			d.save = nil
		} else if ctx.Err() == nil && d.timer == nil {
			d.timer = time.NewTimer(draftFlushInterval)
		}
		return
	}
	d.published = &text
	d.nextWrite = time.Now().Add(draftFlushInterval)
	if text == "" {
		d.nextWrite = time.Time{}
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
