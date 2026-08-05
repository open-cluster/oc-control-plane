package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	relayv1 "github.com/open-cluster/oc-relay/gen/go/opencluster/relay/v1"

	"github.com/open-cluster/oc-control-plane/internal/config"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/tenancy"
)

// The harness for the investigation suite: the assembled process, a real database, a real relay
// speaking the real protocol against a scripted cluster, and one recorded model boundary.
//
// The model is the ONLY thing faked here. test/e2e/doc.go states the rule this deviates from —
// nothing is a test double, because every component that could be faked is one whose behaviour is
// in question — and the deviation is recorded at the seam itself in
// internal/investigation/reasoner.go. Everything else is the real thing: real HTTP, a real
// Postgres, a real gRPC session, real leases.

const (
	investigationToken     = "operator-token-for-the-investigation-surface"
	investigationOrg       = "org-a"
	investigationNeighbour = "org-neighbour"
	investigationNamespace = "payments"
	investigationWorkload  = "checkout"
	investigationPod       = "checkout-7d9f4b-2qk8x"
	investigationContainer = "server"
)

// testClaimInterval is how often the investigator looks for work in this suite. Production polls
// slowly because investigations arrive a few times an hour and a query per instance per second
// forever is a cost with no reader; a test that waited that out would spend most of its wall clock
// asleep for no assertion's benefit.
const testClaimInterval = 250 * time.Millisecond

// investigationPlane is a control plane an engineer can start an investigation through, with a
// relay standing in for a cluster.
type investigationPlane struct {
	*controlPlane
	operator     string
	organization tenancy.Organization
	connection   uuid.UUID
	dsn          string
}

// cluster is what the relay answers with. It is scripted per capability rather than simulated,
// because what is under test is how the control plane turns answers into evidence and gaps — not
// Kubernetes.
type cluster struct {
	workload *relayv1.KubernetesWorkloadRuntimeResultV1
	events   *relayv1.KubernetesNamespaceEventsResultV1
	logs     *relayv1.KubernetesContainerLogsResultV1
	// redaction is what the relay's own enforcement point reports it masked, attached to every
	// result this cluster answers with. It rides on the envelope rather than per capability
	// because that is where the Relay puts it: one enforcement point, one report.
	redaction *relayv1.RedactionReport
	// failing names capabilities the relay reports a failure for rather than a result.
	failing map[string]relayv1.JobFailure_Kind
}

// healthyCluster is a workload whose pods are crash-looping, which is the shape the first
// investigation exists to explain.
func healthyCluster() *cluster {
	readAt := timestamppb.New(time.Now().Add(-time.Minute))
	return &cluster{
		workload: &relayv1.KubernetesWorkloadRuntimeResultV1{
			Outcome: relayv1.KubernetesReadOutcome_KUBERNETES_READ_OUTCOME_SUCCESS,
			Workload: &relayv1.KubernetesWorkloadSummary{
				Kind: "deployment", Name: investigationWorkload,
				Namespace: investigationNamespace, Uid: "3f2b1a44-uid",
				DesiredReplicas: 3, ReadyReplicas: 0, AvailableReplicas: 0, UpdatedReplicas: 3,
				Generation: 7, ObservedGeneration: 7,
				ContainerImages: []string{"registry.internal/checkout:2.9.1"},
				SelectorSummary: "app=checkout",
			},
			Pods: []*relayv1.KubernetesPodRuntime{{
				Name: investigationPod, Phase: "Running", NodeName: "node-3", Ready: false,
				Containers: []*relayv1.KubernetesContainerRuntime{{
					Name: investigationContainer, Image: "registry.internal/checkout:2.9.1",
					Ready: false, RestartCount: 9,
					State: &relayv1.KubernetesContainerState{
						Kind:          relayv1.KubernetesContainerState_KIND_WAITING,
						WaitingReason: "CrashLoopBackOff",
					},
					LastTermination: &relayv1.KubernetesContainerTermination{
						Reason: "Error", ExitCode: 1, FinishedAt: readAt,
					},
				}},
			}},
			ReturnedPodCount: 1, Complete: true, ReadAt: readAt, AppliedMaxPods: 50,
		},
		events: &relayv1.KubernetesNamespaceEventsResultV1{
			Outcome: relayv1.KubernetesEventsOutcome_KUBERNETES_EVENTS_OUTCOME_SUCCESS,
			Events: []*relayv1.KubernetesEvent{{
				Type: "Warning", Reason: "BackOff",
				Message:         "Back-off restarting failed container server in pod " + investigationPod,
				InvolvedObject:  &relayv1.KubernetesInvolvedObject{Kind: "Pod", Name: investigationPod},
				SourceComponent: "kubelet", Count: 9,
				FirstSeenAt: timestamppb.New(time.Now().Add(-40 * time.Minute)),
				LastSeenAt:  timestamppb.New(time.Now().Add(-2 * time.Minute)),
			}, {
				Type: "Normal", Reason: "ScalingReplicaSet",
				Message:         "Scaled up replica set checkout-7d9f4b to 3",
				InvolvedObject:  &relayv1.KubernetesInvolvedObject{Kind: "Deployment", Name: investigationWorkload},
				SourceComponent: "deployment-controller", Count: 1,
				FirstSeenAt: timestamppb.New(time.Now().Add(-45 * time.Minute)),
				LastSeenAt:  timestamppb.New(time.Now().Add(-45 * time.Minute)),
			}},
			ReturnedEventCount: 2, Complete: true, ReadAt: readAt, AppliedMaxEvents: 200,
			AppliedRetentionHorizon: durationpb.New(time.Hour),
		},
		logs: &relayv1.KubernetesContainerLogsResultV1{
			Outcome: relayv1.KubernetesLogsOutcome_KUBERNETES_LOGS_OUTCOME_SUCCESS,
			Lines: []*relayv1.KubernetesLogLine{{
				At:      timestamppb.New(time.Now().Add(-3 * time.Minute)),
				Content: `level=fatal msg="dial tcp 10.4.0.17:5432: connect: connection refused"`,
			}},
			ReturnedLineCount: 1, ReturnedByteCount: 74, Complete: true,
			ReadAt: readAt, AppliedMaxLines: 2000, AppliedMaxBytes: 262144,
		},
		failing: map[string]relayv1.JobFailure_Kind{},
	}
}

// answer builds what the relay sends back for one assignment.
func (c *cluster) answer(assignment *relayv1.JobAssignment) *relayv1.RelayToControl {
	if kind, failing := c.failing[assignment.GetCapabilityId()]; failing {
		return &relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
			JobResult: &relayv1.JobResult{
				JobId:      assignment.GetJobId(),
				LeaseEpoch: assignment.GetLeaseEpoch(),
				Outcome:    &relayv1.JobResult_Failure{Failure: &relayv1.JobFailure{Kind: kind}},
			},
		}}
	}

	result := &relayv1.CapabilityResult{Redaction: c.redaction}
	switch assignment.GetCapabilityId() {
	case "kubernetes.workload.runtime":
		result.Result = &relayv1.CapabilityResult_KubernetesWorkloadRuntimeV1{
			KubernetesWorkloadRuntimeV1: c.workload,
		}
	case "kubernetes.namespace.events":
		result.Result = &relayv1.CapabilityResult_KubernetesNamespaceEventsV1{
			KubernetesNamespaceEventsV1: c.events,
		}
	case "kubernetes.container.logs":
		result.Result = &relayv1.CapabilityResult_KubernetesContainerLogsV1{
			KubernetesContainerLogsV1: c.logs,
		}
	}
	return &relayv1.RelayToControl{Message: &relayv1.RelayToControl_JobResult{
		JobResult: &relayv1.JobResult{
			JobId:               assignment.GetJobId(),
			LeaseEpoch:          assignment.GetLeaseEpoch(),
			Outcome:             &relayv1.JobResult_Result{Result: result},
			ExecutionDurationMs: 11,
		},
	}}
}

// startInvestigationPlane assembles everything a case needs: the operator surface, the relay
// endpoint, an enrolled relay answering from the scripted cluster, and the recorded reasoner.
func startInvestigationPlane(
	t *testing.T, reasoner investigation.Reasoner, answering *cluster, controls investigation.Controls,
) *investigationPlane {
	t.Helper()
	return startInvestigationPlaneWith(t, answering, wiring{
		reasoner:             reasoner,
		controls:             controls,
		investigatorInterval: testClaimInterval,
	}, nil)
}

// startInvestigationPlaneConfigured is the same plane with the model boundary coming from
// CONFIGURATION rather than from an injection. It is the path a deployment takes, and the only one
// available to anything that runs the control plane as a child process.
func startInvestigationPlaneConfigured(
	t *testing.T, answering *cluster, adjust func(*config.Config),
) *investigationPlane {
	t.Helper()
	return startInvestigationPlaneWith(
		t, answering, wiring{investigatorInterval: testClaimInterval}, adjust)
}

func startInvestigationPlaneWith(
	t *testing.T, answering *cluster, replace wiring, adjust func(*config.Config),
) *investigationPlane {
	t.Helper()

	operatorAddress := freeAddress(t)
	relayAddress := freeAddress(t)
	var dsn string

	plane := startControlPlaneRunning(t, func(cfg *config.Config) {
		cfg.OperatorAddress = operatorAddress
		cfg.RelayAddress = relayAddress
		cfg.RelaySPKIPins = []string{base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))}
		digest := sha256.Sum256([]byte(investigationToken))
		cfg.OperatorTokenDigest = digest[:]
		// The bootstrap credential is bound to ONE organization, which is the difference
		// between it and the ambient root token it replaces. A request naming the neighbour
		// below is now refused by the authorization middleware before it reaches a query — the
		// cross-tenant assertions in these tests assert that refusal rather than a scoped query.
		cfg.OperatorTokenOrganization = investigationOrg
		// The neighbour shares this placement deliberately. An organization with no placement fails
		// before any query runs, which would leave the cross-tenant assertions passing against an
		// implementation with no scoping at all.
		cfg.Assignments[investigationNeighbour] = "shared"
		dsn = cfg.Placements["shared"]
		if adjust != nil {
			adjust(cfg)
		}
	}, replace)

	connection := dialRelay(t, relayAddress)
	credentials := registerRelay(t, connection, dsn, investigationOrg)
	placements := openPlacement(t, dsn)
	organization := namedOrganization(t, investigationOrg)

	if answering != nil {
		serveCluster(t, connection, credentials, answering)
	}

	return &investigationPlane{
		controlPlane: plane,
		operator:     operatorAddress,
		organization: organization,
		connection:   evidenceConnection(t, placements, organization, credentials.registration),
		dsn:          dsn,
	}
}

// serveCluster runs the relay for the duration of the test: it takes every assignment and answers
// from the script. It is a goroutine rather than a step in each test because the investigator
// dispatches when it chooses to, and a test that had to be waiting at the right moment would be
// testing its own timing.
func serveCluster(
	t *testing.T, connection *grpc.ClientConn, credentials relayCredentials, answering *cluster,
) {
	t.Helper()

	stream := connectSession(t, connection, investigationOrg, credentials)
	awaitSessionAccepted(t, stream)

	answered := make(chan struct{})
	// Closing the connection is what ends the receive loop, and it has to happen inside this
	// cleanup rather than being left to the stream's own deadline. Cleanups run last-registered
	// first, so a cleanup that only waited would run BEFORE the one that cancels the stream and
	// would sit there until the deadline expired — which is a test that passes and takes ninety
	// seconds to do it.
	t.Cleanup(func() {
		_ = connection.Close()
		<-answered
	})

	go func() {
		defer close(answered)
		for {
			message, err := stream.Recv()
			if err != nil {
				return
			}
			assignment := message.GetJobAssignment()
			if assignment == nil {
				continue
			}
			if sendErr := stream.Send(answering.answer(assignment)); sendErr != nil {
				return
			}
		}
	}()
}

func (p *investigationPlane) base() string {
	return "http://" + p.operator + "/operator/v1/organizations/" + investigationOrg +
		"/investigations"
}

func (p *investigationPlane) baseFor(organization string) string {
	return "http://" + p.operator + "/operator/v1/organizations/" + organization +
		"/investigations"
}

// call sends an authenticated operator request and returns the status, the body and the headers.
func (p *investigationPlane) call(
	t *testing.T, method, url string, body any, headers map[string]string,
) (int, string, http.Header) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+investigationToken)
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("calling %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()

	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the response from %s: %v", url, err)
	}
	return response.StatusCode, string(answer), response.Header
}

// openInvestigation starts a case the way an engineer does: a Connection, a scope and a window.
func (p *investigationPlane) openInvestigation(t *testing.T, window time.Duration) summaryBody {
	t.Helper()

	status, body, _ := p.call(t, http.MethodPost, p.base(), map[string]any{
		"connectionId": p.connection.String(),
		"namespace":    investigationNamespace,
		"workloadKind": "deployment",
		"workloadName": investigationWorkload,
		"windowStart":  time.Now().Add(-window).UTC().Format(time.RFC3339),
		"windowEnd":    time.Now().UTC().Format(time.RFC3339),
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("opening an investigation = %d: %s", status, body)
	}

	var opened summaryBody
	decodeInto(t, body, &opened)
	return opened
}

// awaitTerminal polls the summary until the case finishes, which is what a client does.
func (p *investigationPlane) awaitTerminal(t *testing.T, id string) summaryBody {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for {
		summary := p.summary(t, id)
		if summary.Investigation.Terminal {
			return summary
		}
		if time.Now().After(deadline) {
			t.Fatalf("the investigation never reached a terminal state; it is %s\nlogs:\n%s",
				summary.Investigation.Lifecycle, p.logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (p *investigationPlane) summary(t *testing.T, id string) summaryBody {
	t.Helper()

	status, body, _ := p.call(t, http.MethodGet, p.base()+"/"+id, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reading the summary = %d: %s", status, body)
	}
	var summary summaryBody
	decodeInto(t, body, &summary)
	return summary
}

// section reads one of a case's collections and decodes it into the shape the caller wants.
func (p *investigationPlane) section(t *testing.T, id, name string, into any) {
	t.Helper()

	status, body, _ := p.call(t, http.MethodGet, p.base()+"/"+id+"/"+name, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("reading %s = %d: %s", name, status, body)
	}
	decodeInto(t, body, into)
}

// These mirror what the surface sends. They are spelled out rather than decoded into a map so that
// a renamed field breaks here, where the contract is asserted.

type summaryBody struct {
	Investigation identityBody `json:"investigation"`
	CurrentRound  *roundBody   `json:"currentRound"`
	Outcome       *outcomeBody `json:"outcome"`
	Counts        countsBody   `json:"counts"`
	Spend         spendBody    `json:"spend"`
}

type identityBody struct {
	ID            string `json:"id"`
	EnvironmentID string `json:"environmentId"`
	Scope         struct {
		ConnectionID string `json:"connectionId"`
		Namespace    string `json:"namespace"`
		WorkloadKind string `json:"workloadKind"`
		WorkloadName string `json:"workloadName"`
	} `json:"scope"`
	Lifecycle    string `json:"lifecycle"`
	Running      bool   `json:"running"`
	Terminal     bool   `json:"terminal"`
	CaseVersion  int64  `json:"caseVersion"`
	CurrentRound int    `json:"currentRound"`
}

type roundBody struct {
	ID      string `json:"id"`
	Ordinal int    `json:"ordinal"`
	Outcome string `json:"outcome"`
	Brief   *struct {
		Resource struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			UID       string `json:"uid"`
			Resolved  bool   `json:"resolved"`
		} `json:"resource"`
		RecentChanges []struct {
			Summary  string `json:"summary"`
			Evidence string `json:"evidenceId"`
		} `json:"recentChanges"`
		Topology []struct {
			Pod      string `json:"pod"`
			Node     string `json:"node"`
			Evidence string `json:"evidenceId"`
		} `json:"topology"`
		AvailableCapabilities []struct {
			CapabilityID string `json:"capabilityId"`
		} `json:"availableCapabilities"`
		Coverage    []coverageBody `json:"coverage"`
		AssembledAt time.Time      `json:"assembledAt"`
	} `json:"brief"`
	Controls struct {
		MaxRequests       int `json:"maxRequests"`
		MaxAdaptivePasses int `json:"maxAdaptivePasses"`
	} `json:"controls"`
	Plan struct {
		Template string `json:"template"`
		Intended []struct {
			CapabilityID string `json:"capabilityId"`
			Purpose      string `json:"purpose"`
		} `json:"intended"`
	} `json:"plan"`
	Versions struct {
		Planner      string `json:"planner"`
		Model        string `json:"model"`
		Investigator string `json:"investigator"`
	} `json:"versions"`
}

type outcomeBody struct {
	ID                 string      `json:"id"`
	Round              int         `json:"round"`
	Kind               string      `json:"kind"`
	Statement          string      `json:"statement"`
	Supporting         []claimBody `json:"supporting"`
	Contradicting      []claimBody `json:"contradicting"`
	AffectedScope      []claimBody `json:"affectedScope"`
	RelevantGaps       []string    `json:"relevantCoverageGapIds"`
	Unresolved         []string    `json:"unresolvedHypothesisIds"`
	IndependentSources int         `json:"independentSources"`
	Superseded         bool        `json:"superseded"`
}

type claimBody struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Evidence  []string `json:"evidenceIds"`
}

type countsBody struct {
	Rounds     int `json:"rounds"`
	Evidence   int `json:"evidence"`
	Timeline   int `json:"timeline"`
	Gaps       int `json:"coverageGaps"`
	Hypotheses int `json:"hypotheses"`
	Activity   int `json:"activity"`
	Outcomes   int `json:"outcomes"`
}

type spendBody struct {
	Tokens     int64 `json:"tokens"`
	MicroCents int64 `json:"microCents"`
	DurationMS int64 `json:"durationMs"`
}

type coverageBody struct {
	CapabilityID string `json:"capabilityId"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Evidence     int    `json:"evidence"`
	IsGap        bool   `json:"isGap"`
}

type evidenceSectionBody struct {
	Items []struct {
		ID           string `json:"id"`
		Ordinal      int    `json:"ordinal"`
		CapabilityID string `json:"capabilityId"`
		Source       string `json:"sourceConnectionId"`
		Statement    string `json:"statement"`
		Content      string `json:"content"`
		Absence      bool   `json:"absence"`
		Trust        string `json:"trust"`
		OnTimeline   bool   `json:"onTimeline"`
		Certificate  *struct {
			PaginationComplete bool `json:"paginationComplete"`
			FullyAuthorized    bool `json:"fullyAuthorized"`
			Certifies          bool `json:"certifiesAbsence"`
		} `json:"completenessCertificate"`
	} `json:"items"`
	Next        string `json:"next"`
	CaseVersion int64  `json:"caseVersion"`
}

type gapSectionBody struct {
	Items []struct {
		ID           string `json:"id"`
		Cause        string `json:"cause"`
		CapabilityID string `json:"capabilityId"`
		Subject      string `json:"subject"`
		Consequence  string `json:"consequence"`
	} `json:"items"`
	CaseVersion int64 `json:"caseVersion"`
}

type hypothesisSectionBody struct {
	Items []struct {
		ID        string `json:"id"`
		Ordinal   int    `json:"ordinal"`
		Statement string `json:"statement"`
		Falsifies string `json:"falsifies"`
		State     string `json:"state"`
	} `json:"items"`
	CaseVersion int64 `json:"caseVersion"`
}

type activitySectionBody struct {
	Items []struct {
		ID                     string `json:"id"`
		Pass                   int    `json:"pass"`
		CapabilityID           string `json:"capabilityId"`
		JustifyingHypothesisID string `json:"justifyingHypothesisId"`
		Reason                 string `json:"reason"`
		State                  string `json:"state"`
		Refusal                string `json:"refusal"`
	} `json:"items"`
	CaseVersion int64 `json:"caseVersion"`
}

type coverageSectionBody struct {
	Items       []coverageBody `json:"items"`
	CaseVersion int64          `json:"caseVersion"`
}

type caseFileBody struct {
	Investigation identityBody          `json:"investigation"`
	CaseVersion   int64                 `json:"caseVersion"`
	Rounds        []roundBody           `json:"rounds"`
	Hypotheses    []struct{ ID string } `json:"hypotheses"`
	Evidence      []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	} `json:"evidence"`
	Timeline []struct {
		ID string `json:"id"`
	} `json:"timeline"`
	Gaps []struct {
		ID string `json:"id"`
	} `json:"coverageGaps"`
	Activity []struct {
		ID string `json:"id"`
	} `json:"activity"`
	Outcomes []outcomeBody `json:"outcomes"`
}

type investigationListBody struct {
	Investigations []struct {
		Investigation    identityBody `json:"investigation"`
		Counts           countsBody   `json:"counts"`
		OutcomeKind      string       `json:"outcomeKind"`
		OutcomeStatement string       `json:"outcomeStatement"`
	} `json:"investigations"`
	Next string `json:"next"`
}
