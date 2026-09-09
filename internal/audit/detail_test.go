package audit

import (
	"strings"
	"testing"
)

func TestEventBoundedUsesActionMetadataBounds(t *testing.T) {
	overlong := strings.Repeat("7", MaxDetailValueLength+20)
	event := Event{
		Action: ActionPolicyChanged,
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
			"sessionLifetimeSeconds": "retired deployment-owned setting",
			"unrelated":              "must not land",
		},
	}

	bounded := event.Bounded()

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
	if !ok || len(afterRetention) > MaxDetailValueLength ||
		!strings.HasSuffix(afterRetention, "...") {
		t.Fatalf("after retention metadata was not bounded: %#v", after)
	}
	if _, ok := bounded.Detail["sessionLifetimeSeconds"]; ok {
		t.Fatalf("retired deployment-owned policy metadata survived: %#v", bounded.Detail)
	}
	if _, ok := bounded.Detail["unrelated"]; ok || len(before) != 1 || len(after) != 1 {
		t.Fatalf("unexpected metadata survived: %#v", bounded.Detail)
	}
}

func TestDetailSafeBoundsNestedMetadata(t *testing.T) {
	detail := Detail{
		"context": map[string]any{
			"note":     strings.Repeat("a", MaxDetailValueLength+20),
			"apiToken": "must not land",
		},
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
	if len(note) > MaxDetailValueLength || !strings.HasSuffix(note, "...") {
		t.Fatalf("note was not bounded: length=%d suffix=%q", len(note), note[len(note)-3:])
	}
	if _, ok := context["apiToken"]; ok {
		t.Fatalf("credential-shaped nested metadata survived: %#v", context)
	}
}
