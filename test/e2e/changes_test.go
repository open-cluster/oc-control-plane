package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type changeEventRow struct {
	changeKind int16
	objectName string
	fields     string
}

func (h *harness) changeEvents(ctx context.Context, t *testing.T) []changeEventRow {
	t.Helper()
	rows, err := h.truth.pool.Query(ctx, `
		SELECT change_kind, object_name, fields::text
		  FROM change_event
		 WHERE integration_id = $1 AND namespace = $2 AND change_kind <> 1
		 ORDER BY change_event_id`, h.integration, fixtureNamespace)
	if err != nil {
		t.Fatalf("reading the change history: %v", err)
	}
	defer rows.Close()
	var read []changeEventRow
	for rows.Next() {
		var row changeEventRow
		if err = rows.Scan(&row.changeKind, &row.objectName, &row.fields); err != nil {
			t.Fatalf("reading a change history row: %v", err)
		}
		read = append(read, row)
	}
	return read
}

func (h *harness) awaitChangeBaseline(t *testing.T) {
	t.Helper()
	h.await(t, "the change history's baseline", 2*time.Minute, func(ctx context.Context) (bool, error) {
		var baselined bool
		err := h.truth.pool.QueryRow(ctx, `
			SELECT baseline_at IS NOT NULL FROM change_scope
			 WHERE integration_id = $1`, h.integration).Scan(&baselined)
		if err != nil {
			return false, err
		}
		return baselined, nil
	})
}

func (h *harness) awaitConfirmations(t *testing.T, after time.Time, ticks int) {
	t.Helper()
	deadline := time.Duration(ticks)*10*time.Second + 30*time.Second
	h.await(t, "further synchronization ticks", deadline, func(ctx context.Context) (bool, error) {
		var confirmed time.Time
		err := h.truth.pool.QueryRow(ctx, `
			SELECT coalesce(last_confirmed_at, to_timestamp(0)) FROM change_scope
			 WHERE integration_id = $1`, h.integration).Scan(&confirmed)
		if err != nil {
			return false, err
		}
		return confirmed.After(after.Add(time.Duration(ticks) * 2 * time.Second)), nil
	})
}

func TestProof_AClusterChangeBecomesAChangeEventAndStatusChurnDoesNot(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h.awaitChangeBaseline(t)

	pod, err := h.cluster.podFor(ctx, fixtureWorkload)
	if err != nil {
		t.Fatalf("finding the settled workload's pod: %v", err)
	}
	churnStarted := time.Now().UTC()
	if err = h.cluster.client.CoreV1().Pods(fixtureNamespace).
		Delete(ctx, pod, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	h.await(t, "the replacement pod", 2*time.Minute, func(ctx context.Context) (bool, error) {
		replacement, podErr := h.cluster.podFor(ctx, fixtureWorkload)
		return podErr == nil && replacement != pod, nil
	})
	h.awaitConfirmations(t, churnStarted, 3)

	if rows := h.changeEvents(ctx, t); len(rows) != 0 {
		t.Fatalf("status movement produced %d change history rows; the watched field set has widened "+
			"past declared intent, which is how an investigation product becomes a monitoring "+
			"platform: %+v", len(rows), rows)
	}

	deployments := h.cluster.client.AppsV1().Deployments(fixtureNamespace)
	settled, err := deployments.Get(ctx, fixtureWorkload, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the settled workload: %v", err)
	}
	before := settled.Spec.Template.Spec.Containers[0].Image
	after := pauseImage
	if before == after {
		t.Fatalf("the fixture already runs %s; the change would be invisible", after)
	}
	settled.Spec.Template.Spec.Containers[0].Image = after
	if _, err = deployments.Update(ctx, settled, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("changing the image: %v", err)
	}

	var recorded []changeEventRow
	h.await(t, "the image change to reach the change history", 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			recorded = h.changeEvents(ctx, t)
			return len(recorded) > 0, nil
		})

	var moved *changeEventRow
	for i := range recorded {
		if recorded[i].objectName == fixtureWorkload {
			moved = &recorded[i]
		}
	}
	if moved == nil {
		t.Fatalf("the change history recorded changes but none names %s: %+v", fixtureWorkload, recorded)
	}
	if moved.changeKind != 3 {
		t.Fatalf("an image change is a modification, got change_kind %d", moved.changeKind)
	}

	var fields []struct {
		Field  string `json:"field"`
		Before string `json:"before"`
		After  string `json:"after"`
	}
	if err = json.Unmarshal([]byte(moved.fields), &fields); err != nil {
		t.Fatalf("decoding the itemized fields: %v", err)
	}
	named := false
	for _, field := range fields {
		if strings.HasSuffix(field.Field, ".image") {
			named = true
			if field.Before != before || field.After != after {
				t.Fatalf("both values must survive to the row, got %q -> %q", field.Before, field.After)
			}
		}
		lowered := strings.ToLower(field.Field)
		for _, banned := range []string{"status", "ready", "available", "phase", "condition"} {
			if strings.Contains(lowered, banned) {
				t.Fatalf("the change history itemized %q, which is state rather than declared intent", field.Field)
			}
		}
	}
	if !named {
		t.Fatalf("the row does not name the image field that moved: %+v", fields)
	}
}

const pauseImage = "registry.k8s.io/pause:3.10"
