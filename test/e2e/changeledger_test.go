package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The property that keeps the change ledger an investigation tool rather than a monitoring
// platform, provable only end to end: a change made in a REAL cluster becomes a durable
// ledger row naming the field that moved with both values — and status movement in the same
// namespace produces no row at all.
//
// The second half is the important one and it is written so that WIDENING THE WATCHED FIELD
// SET MAKES IT FAIL: deleting a pod churns ready counts, phases and conditions through the
// same objects the ledger watches, and a build that recorded any of that produces a row this
// test refuses.

// ledgerRow is one change_ledger entry as this test reads it back.
type ledgerRow struct {
	changeKind int16
	objectName string
	fields     string
}

func (h *harness) ledgerChanges(ctx context.Context, t *testing.T) []ledgerRow {
	t.Helper()
	rows, err := h.truth.pool.Query(ctx, `
		SELECT change_kind, object_name, fields::text
		  FROM change_ledger
		 WHERE connection_id = $1 AND namespace = $2 AND change_kind <> 1
		 ORDER BY entry_id`, h.connection, fixtureNamespace)
	if err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	defer rows.Close()
	var read []ledgerRow
	for rows.Next() {
		var row ledgerRow
		if err = rows.Scan(&row.changeKind, &row.objectName, &row.fields); err != nil {
			t.Fatalf("reading a ledger row: %v", err)
		}
		read = append(read, row)
	}
	return read
}

func (h *harness) awaitLedgerBaseline(t *testing.T) {
	t.Helper()
	h.await(t, "the ledger's baseline", 2*time.Minute, func(ctx context.Context) (bool, error) {
		var baselined bool
		err := h.truth.pool.QueryRow(ctx, `
			SELECT baseline_at IS NOT NULL FROM change_ledger_scope
			 WHERE connection_id = $1`, h.connection).Scan(&baselined)
		if err != nil {
			return false, nil // the scope row itself may not exist yet
		}
		return baselined, nil
	})
}

// awaitConfirmations waits until the relay has confirmed at least `ticks` further completed
// synchronizations, so an assertion of silence is an assertion about ticks that actually ran.
func (h *harness) awaitConfirmations(t *testing.T, after time.Time, ticks int) {
	t.Helper()
	deadline := time.Duration(ticks)*10*time.Second + 30*time.Second
	h.await(t, "further synchronization ticks", deadline, func(ctx context.Context) (bool, error) {
		var confirmed time.Time
		err := h.truth.pool.QueryRow(ctx, `
			SELECT coalesce(last_confirmed_at, to_timestamp(0)) FROM change_ledger_scope
			 WHERE connection_id = $1`, h.connection).Scan(&confirmed)
		if err != nil {
			return false, nil
		}
		return confirmed.After(after.Add(time.Duration(ticks) * 2 * time.Second)), nil
	})
}

func TestProof_AClusterChangeBecomesALedgerRowAndStatusChurnDoesNot(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	h.awaitLedgerBaseline(t)

	// STATUS CHURN FIRST. Deleting the settled workload's pod makes the deployment's ready
	// count fall and recover, pod phases move, and conditions rewrite — everything a
	// monitoring platform would record and this ledger must not.
	pod, err := h.cluster.podFor(ctx, fixtureWorkload)
	if err != nil {
		t.Fatalf("finding the settled workload's pod: %v", err)
	}
	churnStarted := time.Now().UTC()
	if err = h.cluster.client.CoreV1().Pods(fixtureNamespace).
		Delete(ctx, pod, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the pod: %v", err)
	}
	// A deleted Deployment pod is REPLACED under a fresh name, not restarted, so the wait
	// is for a differently-named pod — and that replacement is itself more status churn.
	h.await(t, "the replacement pod", 2*time.Minute, func(ctx context.Context) (bool, error) {
		replacement, podErr := h.cluster.podFor(ctx, fixtureWorkload)
		return podErr == nil && replacement != pod, nil
	})
	h.awaitConfirmations(t, churnStarted, 3)

	if rows := h.ledgerChanges(ctx, t); len(rows) != 0 {
		t.Fatalf("status movement produced %d ledger rows; the watched field set has widened "+
			"past declared intent, which is how an investigation product becomes a monitoring "+
			"platform: %+v", len(rows), rows)
	}

	// NOW A REAL CHANGE: the image moves. This is declared intent, and it is exactly what an
	// on-call engineer at 03:40 needs the platform to have remembered.
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

	var recorded []ledgerRow
	h.await(t, "the image change to reach the ledger", 2*time.Minute,
		func(ctx context.Context) (bool, error) {
			recorded = h.ledgerChanges(ctx, t)
			return len(recorded) > 0, nil
		})

	var moved *ledgerRow
	for i := range recorded {
		if recorded[i].objectName == fixtureWorkload {
			moved = &recorded[i]
		}
	}
	if moved == nil {
		t.Fatalf("the ledger recorded changes but none names %s: %+v", fixtureWorkload, recorded)
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
		// The belt on the second half: no itemized field may ever speak status vocabulary.
		lowered := strings.ToLower(field.Field)
		for _, banned := range []string{"status", "ready", "available", "phase", "condition"} {
			if strings.Contains(lowered, banned) {
				t.Fatalf("the ledger itemized %q, which is state rather than declared intent", field.Field)
			}
		}
	}
	if !named {
		t.Fatalf("the row does not name the image field that moved: %+v", fields)
	}
}

// pauseImage is a second pinned image the fixture does not run, so an image change has a
// visible before and after. Whether the cluster can pull it is irrelevant: the ledger
// records DECLARED intent, and the declaration happened the moment the spec moved.
const pauseImage = "registry.k8s.io/pause:3.10"
