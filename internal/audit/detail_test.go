package audit

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEventBoundedUsesOneSanitizerForEveryAction(t *testing.T) {
	overlong := strings.Repeat("7", maxAuditTextLength+20)
	event := Event{
		Actor:         Actor{ID: overlong, DisplayName: overlong},
		Action:        ActionPolicyChanged,
		Target:        Target{ID: overlong},
		SourceAddress: overlong,
		RequestID:     overlong,
		Detail: Detail{
			"before": map[string]any{
				"auditRetentionDays": 30,
				"apiToken":           "must not land",
				"ignored":            "must not land",
			},
			"after": map[string]any{
				"auditRetentionDays": overlong,
				"password":           "must not land",
			},
			"unrelated": "safe context",
		},
	}

	bounded := event.Bounded()
	if len(bounded.Actor.ID) > maxAuditTextLength ||
		len(bounded.Actor.DisplayName) > maxAuditTextLength ||
		len(bounded.Target.ID) > maxAuditTextLength {
		t.Fatal("general audit text exceeded its bound")
	}
	if len(bounded.SourceAddress) > maxAuditMetadataLength ||
		len(bounded.RequestID) > maxAuditMetadataLength {
		t.Fatal("request metadata exceeded its bound")
	}

	before, ok := bounded.Detail["before"].(Detail)
	if !ok {
		t.Fatalf("before metadata = %#v", bounded.Detail["before"])
	}
	after, ok := bounded.Detail["after"].(Detail)
	if !ok {
		t.Fatalf("after metadata = %#v", bounded.Detail["after"])
	}
	if before["auditRetentionDays"] != 30 {
		t.Fatalf("before retention metadata was not preserved: %#v", before)
	}
	afterRetention, ok := after["auditRetentionDays"].(string)
	if !ok || len(afterRetention) > maxAuditTextLength ||
		!strings.HasSuffix(afterRetention, "...") {
		t.Fatalf("after retention metadata was not bounded: %#v", after)
	}
	if before["ignored"] != "must not land" || bounded.Detail["unrelated"] != "safe context" {
		t.Fatalf("safe action metadata was discarded: %#v", bounded.Detail)
	}
	if _, ok := before["apiToken"]; ok {
		t.Fatalf("credential-shaped metadata survived: %#v", before)
	}
}

func TestDetailSafeBoundsNestedMetadata(t *testing.T) {
	detail := Detail{
		"context": map[string]any{
			"note":     strings.Repeat("界", maxAuditTextLength+20),
			"apiToken": "must not land",
		},
	}
	for index := range maxAuditDetailEntries + 2 {
		detail[fmt.Sprintf("field%02d", index)] = index
	}

	safe := detail.Safe()

	context, ok := safe["context"].(Detail)
	if !ok {
		t.Fatalf("context metadata = %#v", safe["context"])
	}
	note, ok := context["note"].(string)
	if !ok {
		t.Fatalf("note metadata = %#v", context["note"])
	}
	if utf8.RuneCountInString(note) != maxAuditTextLength ||
		!strings.HasSuffix(note, "...") || !utf8.ValidString(note) {
		t.Fatalf("note was not bounded: runes=%d suffix=%q", utf8.RuneCountInString(note), note[len(note)-3:])
	}
	if _, ok := context["apiToken"]; ok {
		t.Fatalf("credential-shaped nested metadata survived: %#v", context)
	}
	if len(safe) != maxAuditDetailEntries || safe["field00"] != 0 {
		t.Fatalf("detail entry selection was not bounded and deterministic: %#v", safe)
	}
}
