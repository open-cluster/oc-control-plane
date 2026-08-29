package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/auth/tenancy"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/integrations/kubernetes"
	"github.com/open-cluster/oc-control-plane/internal/relay/capability"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

func TestFailedWebhookDeliveryIsVisibleTenantScopedAndReplayable(t *testing.T) {
	plane := startIntegrationPlane(t)
	created := plane.createAlertmanager(t, "Terminal webhook source")
	if status, body := plane.deliver(t, created.Integration.ID, created.WebhookSecret,
		alertmanagerPayload("terminal-webhook", "terminal-group")); status != http.StatusAccepted {
		t.Fatalf("accepting the delivery = %d: %s", status, body)
	}

	ctx := context.Background()
	database, err := pgx.Connect(ctx, plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close(ctx) }()
	var workID, deliveryID string
	if err = database.QueryRow(ctx, `
		UPDATE webhook_work
		   SET status = 4, attempts = 8, lease_owner = '', lease_expires_at = NULL,
		       failure_class = 'provider-work-failed',
		       failure_message = 'the accepted webhook work could not be applied',
		       updated_at = now()
		 WHERE org_id = $1 AND integration_id = $2
		 RETURNING work_id, delivery_id`, surfaceOrg, created.Integration.ID).Scan(&workID, &deliveryID); err != nil {
		t.Fatalf("recording a terminal work item: %v", err)
	}

	base := plane.base(surfaceOrg) + "/webhook-deliveries"
	status, body := plane.call(t, http.MethodGet, base, nil)
	if status != http.StatusOK {
		t.Fatalf("listing terminal work = %d: %s", status, body)
	}
	var listed struct {
		Items []struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			Attempts     int    `json:"attempts"`
			FailureClass string `json:"failureCategory"`
		} `json:"items"`
	}
	decodeInto(t, body, &listed)
	if len(listed.Items) != 1 || listed.Items[0].ID != deliveryID || listed.Items[0].Attempts != 8 ||
		listed.Items[0].FailureClass != "provider-work-failed" || listed.Items[0].Status != "failed" {
		t.Fatalf("failed delivery listing = %+v", listed.Items)
	}
	if status, body = plane.call(t, http.MethodGet, base+"?cursor=invalid", nil); status != http.StatusBadRequest {
		t.Fatalf("a malformed terminal-work cursor = %d: %s", status, body)
	}
	if status, body = plane.call(t, http.MethodGet, base+"?sort=receivedAt", nil); status != http.StatusBadRequest {
		t.Fatalf("an unsupported ascending terminal-work order = %d: %s", status, body)
	}
	if status, body = plane.deliver(t, created.Integration.ID, created.WebhookSecret,
		alertmanagerPayload("terminal-webhook-second", "terminal-group-second")); status != http.StatusAccepted {
		t.Fatalf("accepting the second delivery = %d: %s", status, body)
	}
	var secondWorkID, secondDeliveryID string
	if err = database.QueryRow(ctx, `
		UPDATE webhook_work
		   SET status = 4, attempts = 4, lease_owner = '', lease_expires_at = NULL,
		       failure_class = 'provider-work-failed', failure_message = 'safe failure',
		       updated_at = now()
		 WHERE org_id = $1 AND integration_id = $2 AND work_id <> $3
		 RETURNING work_id, delivery_id`, surfaceOrg, created.Integration.ID, workID).
		Scan(&secondWorkID, &secondDeliveryID); err != nil {
		t.Fatalf("recording the second terminal work item: %v", err)
	}
	status, body = plane.call(t, http.MethodGet, base+"?limit=1", nil)
	if status != http.StatusOK {
		t.Fatalf("reading the first bounded terminal-work page = %d: %s", status, body)
	}
	var firstPage struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Next string `json:"next"`
	}
	decodeInto(t, body, &firstPage)
	if len(firstPage.Items) != 1 || firstPage.Items[0].ID != secondDeliveryID || firstPage.Next == "" {
		t.Fatalf("first bounded terminal-work page = %+v", firstPage)
	}
	status, body = plane.call(t, http.MethodGet,
		base+"?limit=1&cursor="+url.QueryEscape(firstPage.Next), nil)
	if status != http.StatusOK {
		t.Fatalf("reading the second bounded terminal-work page = %d: %s", status, body)
	}
	var secondPage struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Next string `json:"next"`
	}
	decodeInto(t, body, &secondPage)
	if len(secondPage.Items) != 1 || secondPage.Items[0].ID != deliveryID || secondPage.Next != "" {
		t.Fatalf("second bounded terminal-work page = %+v", secondPage)
	}
	if status, body = plane.call(t, http.MethodGet, base+"/"+deliveryID, nil); status != http.StatusOK {
		t.Fatalf("reading terminal work = %d: %s", status, body)
	}
	if status, body = plane.call(t, http.MethodGet,
		plane.base(neighbourOrg)+"/webhook-deliveries/"+deliveryID, nil); status != http.StatusNotFound {
		t.Fatalf("reading another organization's work = %d: %s", status, body)
	}
	if status, body = plane.call(t, http.MethodPost, base+"/"+deliveryID+"/replay", nil); status != http.StatusNoContent {
		t.Fatalf("replaying terminal work = %d: %s", status, body)
	}
	var auditedIntegration string
	var auditedWork int
	if err = database.QueryRow(ctx, `
		SELECT detail->>'integrationId', (detail->>'workReplayed')::integer
		  FROM audit_event
		 WHERE org_id = $1 AND action = 'webhook-delivery.replayed' AND target_id = $2`,
		surfaceOrg, deliveryID).Scan(&auditedIntegration, &auditedWork); err != nil ||
		auditedIntegration != created.Integration.ID || auditedWork != 1 {
		t.Fatalf("replay audit omitted delivery context: integration=%q work=%d error=%v",
			auditedIntegration, auditedWork, err)
	}
	if status, body = plane.call(t, http.MethodGet, base+"/"+deliveryID, nil); status != http.StatusOK {
		t.Fatalf("replayed delivery disappeared: %d %s", status, body)
	}
	if status, body = plane.call(t, http.MethodPost, base+"/"+deliveryID+"/replay", nil); status != http.StatusConflict {
		t.Fatalf("replaying nonterminal work = %d: %s", status, body)
	}
}

func TestWebhookDeliveryReplayIsRefusedToEditorsAndViewers(t *testing.T) {
	plane := startIdentityPlane(t)
	admin := bootstrapIdentityAdmin(t, plane, "admin@example.test", "Admin",
		"initial administrator password")
	base := plane.base(identityOrg) + "/webhook-deliveries"
	for _, role := range []string{"editor", "viewer"} {
		email := role + "@example.test"
		password := "a sufficiently long " + role + " password"
		member := plane.call(t, http.MethodPost,
			"http://"+plane.operator+"/api/v1/local-users", map[string]any{
				"email": email, "displayName": role, "role": role, "password": password,
			}, asSession(admin), inOrganization(identityOrg))
		if member.status != http.StatusCreated {
			t.Fatalf("creating %s = %d: %s", role, member.status, member.body)
		}
		signedIn := plane.call(t, http.MethodPost, "http://"+plane.operator+"/api/v1/auth/local/sign-in",
			map[string]any{"organization": identityOrg, "email": email, "password": password})
		if signedIn.status != http.StatusOK {
			t.Fatalf("signing in %s = %d: %s", role, signedIn.status, signedIn.body)
		}
		credential := asSession(sessionCookie(t, signedIn))
		if listed := plane.call(t, http.MethodGet, base, nil, credential); listed.status != http.StatusOK {
			t.Fatalf("%s terminal-work visibility = %d: %s", role, listed.status, listed.body)
		}
		if replayed := plane.call(t, http.MethodPost, base+"/"+uuid.NewString()+"/replay",
			nil, credential); replayed.status != http.StatusForbidden {
			t.Fatalf("%s replay = %d: %s", role, replayed.status, replayed.body)
		}
	}
}

func TestKubernetesWorkloadToolRunsAcrossTheComposedRelayAndDatabase(t *testing.T) {
	plane := startIntegrationPlane(t)
	connection := dialRelay(t, plane.relayAt)
	stream := connectSession(t, connection, surfaceOrg, plane.relay)
	awaitSessionAccepted(t, stream)

	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/integrations",
		map[string]any{
			"type": "kubernetes", "name": "Production Cluster",
			"relayId":       plane.relay.registration.String(),
			"configuration": map[string]any{"namespaceAllowList": "shop"},
		})
	if status != http.StatusCreated {
		t.Fatalf("creating the kubernetes integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	if status, body = plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/integrations/"+created.Integration.ID+"/verify", nil); status != http.StatusOK {
		t.Fatalf("verifying the Relay's advertised reads = %d: %s", status, body)
	}
	database, err := storage.OpenDatabase(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	organization, err := tenancy.NewOrganization(surfaceOrg)
	if err != nil {
		t.Fatal(err)
	}
	integration, err := database.Integration(context.Background(), organization,
		uuid.MustParse(created.Integration.ID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type answer struct {
		result integrations.ToolResult
		err    error
	}
	completed := make(chan answer, 1)
	go func() {
		result, callErr := kubernetes.Definition(kubernetes.RelayExecutor{Database: database}).
			Tools[0].Run(ctx, integrations.ToolRequest{
			Integration: integration,
			Arguments: map[string]any{
				"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout",
				"maxPods": float64(3),
			},
			WindowFrom: time.Now().Add(-time.Hour), WindowUntil: time.Now(),
		})
		completed <- answer{result: result, err: callErr}
	}()
	select {
	case failed := <-completed:
		t.Fatalf("the Relay-backed Tool failed before dispatch: %v (status=%s grants=%v note=%q verified=%s relay=%s)",
			failed.err, integration.Status.String(), integration.VerifyGrants,
			integration.VerifyNote, integration.LastVerifiedAt, integration.RelayID)
	case <-time.After(500 * time.Millisecond):
	}
	assignment := awaitAssignment(t, stream)
	arguments := assignment.GetArguments().GetKubernetesWorkloadRuntimeV1()
	if arguments == nil || arguments.GetNamespace() != "shop" ||
		arguments.GetWorkloadName() != "checkout" || arguments.GetMaxPods() != 3 {
		t.Fatalf("relay assignment arguments = %+v", arguments)
	}
	sendResult(t, stream, assignment.GetJobId(), assignment.GetLeaseEpoch()-1)
	if refused := awaitResultAck(t, stream); refused.GetDisposition() !=
		relayv1.ResultAck_DISPOSITION_STALE_STOP_RESENDING {
		t.Fatalf("a stale Relay result crossed the Tool's lease fence: %v", refused.GetDisposition())
	}
	select {
	case finished := <-completed:
		t.Fatalf("a stale Relay result finished the Tool: %+v error=%v", finished.result, finished.err)
	default:
	}
	if err = stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId: assignment.GetJobId(), LeaseEpoch: assignment.GetLeaseEpoch(),
			Outcome: &relayv1.JobResult_Result{Result: &relayv1.CapabilityResult{
				Result: &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
					KubernetesWorkloadRuntimeV1: &relayv1.KubernetesWorkloadRuntimeResultV1{
						Outcome:          relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
						Pods:             []*relayv1.KubernetesPodRuntime{{Name: "checkout-1", Phase: "Running"}},
						ReturnedPodCount: 1, AppliedMaxPods: 3, Complete: true,
					},
				},
			}},
		},
	}}); err != nil {
		t.Fatalf("sending the real Relay result: %v", err)
	}
	awaitResultAck(t, stream)
	select {
	case got := <-completed:
		if got.err != nil {
			t.Fatalf("executing the Relay-backed Tool: %v", got.err)
		}
		if len(got.result.Sources) != 1 || got.result.Sources[0] != "shop/deployment/checkout" {
			t.Fatalf("result provenance = %+v", got.result.Sources)
		}
	case <-ctx.Done():
		t.Fatalf("the Relay-backed Tool did not finish: %v", ctx.Err())
	}
	pool, err := database.Pool(organization)
	if err != nil {
		t.Fatal(err)
	}
	cancelledContext, cancelRead := context.WithCancel(ctx)
	cancelled := make(chan answer, 1)
	go func() {
		result, readErr := kubernetes.Definition(kubernetes.RelayExecutor{Database: database}).
			Tools[0].Run(cancelledContext, integrations.ToolRequest{
			Integration: integration,
			Arguments: map[string]any{"namespace": "shop", "workloadKind": "Deployment",
				"workloadName": "checkout"},
		})
		cancelled <- answer{result: result, err: readErr}
	}()
	select {
	case failed := <-cancelled:
		t.Fatalf("the cancellable Tool failed before dispatch: %v", failed.err)
	case <-time.After(500 * time.Millisecond):
	}
	cancellable := awaitAssignment(t, stream)
	cancelRead()
	select {
	case stopped := <-cancelled:
		if !errors.Is(stopped.err, context.Canceled) {
			t.Fatalf("cancelling the in-flight Tool returned %v", stopped.err)
		}
	case <-ctx.Done():
		t.Fatalf("the cancelled Tool did not return: %v", ctx.Err())
	}
	cancellation := awaitCancellation(t, stream)
	if cancellation.GetJobId() != cancellable.GetJobId() ||
		cancellation.GetLeaseEpoch() != cancellable.GetLeaseEpoch() {
		t.Fatalf("Tool cancellation addressed another durable execution: %+v", cancellation)
	}
	var jobStatus int16
	var requested bool
	if err = pool.QueryRow(context.Background(), `
		SELECT status, cancel_requested_at IS NOT NULL
		  FROM relay_job WHERE org_id = $1 AND job_id = $2`,
		organization.String(), cancellable.GetJobId()).Scan(&jobStatus, &requested); err != nil || jobStatus != int16(storage.JobLeased) || !requested {
		t.Fatalf("cancellation lost its durable leased Job: status=%d requested=%t error=%v",
			jobStatus, requested, err)
	}
	sendCancelledResult(t, stream, cancellable.GetJobId(), cancellable.GetLeaseEpoch())
	if acknowledged := awaitResultAck(t, stream); acknowledged.GetDisposition() !=
		relayv1.ResultAck_DISPOSITION_RECORDED {
		t.Fatalf("the cancelled Relay outcome was not durably recorded: %v", acknowledged.GetDisposition())
	}
	if err = pool.QueryRow(context.Background(), `
		SELECT status FROM relay_job WHERE org_id = $1 AND job_id = $2`,
		organization.String(), cancellable.GetJobId()).Scan(&jobStatus); err != nil || jobStatus != int16(storage.JobCancelled) {
		t.Fatalf("the cancelled Job did not retain its terminal outcome: status=%d error=%v", jobStatus, err)
	}
	if _, err = pool.Exec(context.Background(), `
		UPDATE integration SET verify_grants = '[]'::jsonb
		 WHERE org_id = $1 AND integration_id = $2`, organization.String(), integration.ID); err != nil {
		t.Fatalf("revoking the Integration's verified read: %v", err)
	}
	_, err = kubernetes.Definition(kubernetes.RelayExecutor{Database: database}).
		Tools[0].Run(ctx, integrations.ToolRequest{
		Integration: integration,
		Arguments: map[string]any{
			"namespace": "shop", "workloadKind": "Deployment", "workloadName": "checkout",
		},
	})
	if !errors.Is(err, storage.ErrJobRefused) {
		t.Fatalf("a stale Tool offer executed after its verified grant was revoked: %v", err)
	}
	var jobs int
	if err = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM relay_job WHERE org_id = $1 AND integration_id = $2`,
		organization.String(), integration.ID).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("revoked verification wrote additional Relay jobs: count=%d error=%v", jobs, err)
	}
}

func TestKubernetesEventAndLogToolsRunAcrossTheComposedRelayAndDatabase(t *testing.T) {
	plane := startIntegrationPlane(t)
	connection := dialRelay(t, plane.relayAt)
	advertised := []string{capability.KubernetesWorkloadRuntime,
		capability.KubernetesNamespaceEvents, capability.KubernetesContainerLogs}
	registration := registerRelay(t, connection, plane.dsn, surfaceOrg, advertised...)
	stream := openStream(t, connection, surfaceOrg, registration)
	descriptors := make([]*relayv1.CapabilityDescriptor, 0, len(advertised))
	for _, name := range advertised {
		descriptors = append(descriptors, &relayv1.CapabilityDescriptor{
			CapabilityId: name, CapabilityVersion: capability.SchemaVersion1,
		})
	}
	if err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_Hello{
		Hello: &relayv1.Hello{ProtocolVersion: protocolVersionUnderTest,
			RelayVersion: "0.1.0-test", MaxConcurrentJobs: 4, Capabilities: descriptors},
	}}); err != nil {
		t.Fatalf("declaring the three Kubernetes reads: %v", err)
	}
	awaitSessionAccepted(t, stream)
	status, body := plane.call(t, http.MethodPost, plane.base(surfaceOrg)+"/integrations",
		map[string]any{"type": "kubernetes", "name": "Observed Cluster",
			"relayId":       registration.registration.String(),
			"configuration": map[string]any{"namespaceAllowList": "shop"}})
	if status != http.StatusCreated {
		t.Fatalf("creating the Kubernetes Integration = %d: %s", status, body)
	}
	var created createdBody
	decodeInto(t, body, &created)
	if status, body = plane.call(t, http.MethodPost,
		plane.base(surfaceOrg)+"/integrations/"+created.Integration.ID+"/verify", nil); status != http.StatusOK {
		t.Fatalf("verifying the Relay's three reads = %d: %s", status, body)
	}
	database, err := storage.OpenDatabase(context.Background(), plane.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	organization, err := tenancy.NewOrganization(surfaceOrg)
	if err != nil {
		t.Fatal(err)
	}
	integration, err := database.Integration(context.Background(), organization,
		uuid.MustParse(created.Integration.ID))
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Truncate(time.Second)
	for _, testCase := range []struct {
		name      string
		index     int
		arguments map[string]any
		result    *relayv1.CapabilityResult
		source    string
	}{
		{name: "namespace events", index: 1,
			arguments: map[string]any{"namespace": "shop", "maxEvents": float64(5)},
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
				KubernetesNamespaceEventsV1: &relayv1.KubernetesNamespaceEventsResultV1{
					Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
					Events: []*relayv1.KubernetesEvent{{
						LastSeenAt: timestamppb.New(until.Add(-time.Minute)),
					}}, ReturnedEventCount: 1, AppliedMaxEvents: 5, Complete: true,
				},
			}}, source: "shop/events"},
		{name: "container logs", index: 2,
			arguments: map[string]any{"namespace": "shop", "podName": "checkout-1",
				"containerName": "api", "maxLines": float64(3), "maxBytes": float64(64)},
			result: &relayv1.CapabilityResult{Result: &relayv1.CapabilityResult_KubernetesContainerLogsV1{
				KubernetesContainerLogsV1: &relayv1.KubernetesContainerLogsResultV1{
					Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
					Lines: []*relayv1.KubernetesLogLine{{
						At: timestamppb.New(until.Add(-time.Minute)), Content: "ready",
					}}, ReturnedLineCount: 1, ReturnedByteCount: 5,
					AppliedMaxLines: 3, AppliedMaxBytes: 64, Complete: true,
				},
			}}, source: "shop/checkout-1/api"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			type completedRead struct {
				result integrations.ToolResult
				err    error
			}
			completed := make(chan completedRead, 1)
			go func() {
				result, readErr := kubernetes.Definition(kubernetes.RelayExecutor{Database: database}).
					Tools[testCase.index].Run(ctx, integrations.ToolRequest{
					Integration: integration, Arguments: testCase.arguments,
					WindowFrom: until.Add(-time.Hour), WindowUntil: until,
				})
				completed <- completedRead{result: result, err: readErr}
			}()
			select {
			case failed := <-completed:
				t.Fatalf("the authorized read failed before dispatch: %v", failed.err)
			case <-time.After(500 * time.Millisecond):
			}
			assignment := awaitAssignment(t, stream)
			if assignment.GetCapabilityId() != advertised[testCase.index] {
				t.Fatalf("assigned Relay read = %q, want %q", assignment.GetCapabilityId(),
					advertised[testCase.index])
			}
			if err := stream.Send(&relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
				JobResult: &relayv1.JobResult{JobId: assignment.GetJobId(),
					LeaseEpoch: assignment.GetLeaseEpoch(),
					Outcome:    &relayv1.JobResult_Result{Result: testCase.result}},
			}}); err != nil {
				t.Fatalf("returning the typed Relay result: %v", err)
			}
			awaitResultAck(t, stream)
			select {
			case got := <-completed:
				if got.err != nil || len(got.result.Sources) != 1 ||
					got.result.Sources[0] != testCase.source {
					t.Fatalf("Relay Tool result = %+v, error = %v", got.result, got.err)
				}
			case <-ctx.Done():
				t.Fatalf("the Relay read did not finish: %v", ctx.Err())
			}
		})
	}
}
