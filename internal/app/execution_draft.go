package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-cluster/oc-control-plane/internal/conversation"
)

const draftFlushInterval = 250 * time.Millisecond

type executionDraft struct {
	save      func(context.Context, string) error
	secret    string
	raw       strings.Builder
	length    int
	published string
	lastWrite time.Time
	timer     *time.Timer
	flushAt   <-chan time.Time
}

func (d *executionDraft) reset(ctx context.Context) {
	d.stopTimer()
	d.raw.Reset()
	d.length = 0
	d.flush(ctx)
}

func (d *executionDraft) append(ctx context.Context, delta string) {
	remaining := conversation.MaxMessageTextLength - d.length
	if d.save == nil || remaining == 0 {
		return
	}
	text := conversation.Bounded(strings.ReplaceAll(delta, "\x00", ""), remaining)
	d.raw.WriteString(text)
	d.length += utf8.RuneCountInString(text)
	if d.flushAt != nil {
		return
	}
	wait := time.Until(d.lastWrite.Add(draftFlushInterval))
	if d.published == "" || wait <= 0 {
		d.flush(ctx)
		return
	}
	if d.timer == nil {
		d.timer = time.NewTimer(wait)
	} else {
		d.timer.Reset(wait)
	}
	d.flushAt = d.timer.C
}

func (d *executionDraft) stopTimer() {
	if d.timer != nil {
		d.timer.Stop()
	}
	d.flushAt = nil
}

func (d *executionDraft) flush(ctx context.Context) {
	if d.save == nil {
		return
	}
	text := conversation.Bounded(safeDraftText(d.raw.String(), d.secret), conversation.MaxMessageTextLength)
	if text == d.published {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := d.save(writeCtx, text); err != nil {
		d.save = nil
		return
	}
	d.published, d.lastWrite = text, time.Now()
}

func safeDraftText(raw, secret string) string {
	// A trailing proper prefix may complete into a secret in the next delta.
	raw = redactModelSecret(raw, secret)
	withhold := 0
	for _, value := range modelSecretForms(secret) {
		for length := min(len(value)-1, len(raw)); length > withhold; length-- {
			if strings.HasSuffix(raw, value[:length]) {
				withhold = length
				break
			}
		}
	}
	return raw[:len(raw)-withhold]
}

func redactModelSecret(text, secret string) string {
	for _, value := range modelSecretForms(secret) {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}

func modelSecretForms(secret string) []string {
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
