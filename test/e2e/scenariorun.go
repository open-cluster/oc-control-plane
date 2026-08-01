package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// One scenario run, end to end, through the real product.
//
// Everything below drives the control plane the way an engineer does — over the operator API —
// rather than by writing rows. A harness that wrote its own rows would prove that the database
// accepts them; what is in question is whether the product, assembled and running, produces an
// explanation worth reading.
//
// The harness is a PROGRAM rather than a test. It has a human in the loop, it calls a paid model,
// and it produces a judgement rather than a pass. Putting it under `go test` would either weaken
// CI or misrepresent what it proves.

// investigationDeadline bounds how long one investigation may take before the run is abandoned.
// It is generous because the path is long and, when a live provider is answering, priced per
// call.
//
// It is not called a budget. CONTEXT.md reserves that word away from Execution limits — the
// numeric bounds on an investigator execution — and this is the harness's patience rather than
// one of the case's own limits, so calling it a budget would collide with the term twice over.
const investigationDeadline = 10 * time.Minute

// scenarioOrganization is the tenant every scenario run belongs to. One is enough: tenant
// isolation is proven in the control plane's own suite, and nothing about an explanation varies
// by tenant.
const scenarioOrganization = "scenario-harness"

// ErrNoLiveProvider reports the one thing this build cannot do.
//
// The specification says the harness always calls the real provider and records the transcript as
// a by-product. There is no live provider in this build — the model boundary has a recorded
// replay and an unavailable stub and nothing else — so a run either replays a recording or says
// this, out loud, rather than quietly scoring something that was never asked of a model.
var ErrNoLiveProvider = errors.New(
	"this build has no live model provider: the Reasoner seam carries a recorded replay and an " +
		"unavailable stub only. Run with recorded transcripts, or build the provider that " +
		"implements investigation.Reasoner against a real API and record from it")

// ModelSource is where a run's reasoning comes from.
type ModelSource struct {
	// TranscriptDir holds one recording per scenario, named <scenario-id>.json. Empty means a
	// live provider was asked for.
	TranscriptDir string
}

// Describe names the source for the artifact, so a scorer reading a replayed run knows they are
// reading a reproduction.
func (m ModelSource) Describe() string {
	if m.TranscriptDir == "" {
		return "live provider"
	}
	return "recorded transcript"
}

// Options is how one invocation of the harness is configured.
type Options struct {
	// Results is where artifacts, ground truth and scores are filed.
	Results *Results
	// Model is where reasoning comes from.
	Model ModelSource
	// CodeVersion identifies the build under evaluation, so a regression is attributable.
	CodeVersion string
	// Progress receives a line per step. A run takes minutes per scenario and silence is
	// indistinguishable from a hang.
	Progress func(format string, args ...any)
}

func (o Options) say(format string, args ...any) {
	if o.Progress != nil {
		o.Progress(format, args...)
	}
}

// RunSet runs every scenario in order and reports what was produced.
//
// A scenario that could not be provisioned is DISCARDED and recorded as discarded; the set
// continues. Silently scoring a run of a different failure than the one declared is the worst
// thing this instrument could do, so the discard is loud and filed, and a discarded run makes the
// set incomplete rather than absent.
func RunSet(ctx context.Context, scenarios []Scenario, options Options) error {
	remove, err := useBuildRoot()
	if err != nil {
		return err
	}
	defer remove()

	options.say("building both halves")
	if _, err = controlPlaneBinary(); err != nil {
		return fmt.Errorf("building the control plane: %w", err)
	}
	if _, err = relayBinary(); err != nil {
		return fmt.Errorf("building the relay: %w", err)
	}

	var discarded int
	for index, scenario := range scenarios {
		options.say("[%d/%d] %s", index+1, len(scenarios), scenario.ID)
		if runErr := RunScenario(ctx, scenario, options); runErr != nil {
			if !errors.Is(runErr, ErrNotProvisioned) {
				return runErr
			}
			discarded++
			options.say("  DISCARDED: %v", runErr)
		}
	}
	if discarded > 0 {
		return fmt.Errorf("%d of %d scenarios were discarded; the set is incomplete and must "+
			"not be read as a result", discarded, len(scenarios))
	}
	return nil
}

// RunScenario provisions one scenario, verifies it reached its declared broken state, runs a real
// investigation against it, and files the artifact and the ground truth apart.
func RunScenario(ctx context.Context, scenario Scenario, options Options) error {
	runID := uuid.NewString()
	started := time.Now()

	// The truth is written FIRST, before anything runs. Writing it afterwards would leave it
	// possible — however unlikely — for what happened to influence what was supposedly declared
	// in advance, and the whole instrument rests on that not being possible.
	truth := GroundTruthOf(scenario, runID, started)
	if _, err := options.Results.WriteGroundTruth(truth); err != nil {
		return err
	}

	transcript, err := transcriptFor(scenario, options.Model)
	if err != nil {
		return err
	}

	run, err := newScenarioRun(ctx, scenario, transcript, options)
	if err != nil {
		return options.discard(truth, err)
	}
	defer run.close()

	options.say("  provisioning and verifying the declared broken state")
	if err = scenario.Prepare(ctx, run.cluster.client); err != nil {
		return options.discard(truth, err)
	}

	options.say("  opening an investigation")
	summary, err := run.investigate(ctx, scenario)
	if err != nil {
		return fmt.Errorf("scenario %s: %w\n%s", scenario.ID, err, run.diagnostics())
	}

	caseFile, err := run.caseFile(ctx, summary.id)
	if err != nil {
		return fmt.Errorf("scenario %s: %w", scenario.ID, err)
	}

	path, err := options.Results.WriteArtifact(Artifact{
		RunID: runID,
		Components: Components{
			CodeVersion:   options.CodeVersion,
			ModelSource:   options.Model.Describe(),
			Model:         summary.versions.Model,
			PromptVersion: summary.versions.PromptVersion,
			SchemaVersion: summary.versions.SchemaVersion,
			Investigator:  summary.versions.Investigator,
		},
		Cost: Cost{
			Tokens: summary.tokens, MicroCents: summary.microCents,
			Requests: summary.requests, ResultKiB: summary.resultBytes / 1024,
		},
		Elapsed:  time.Since(started),
		CaseFile: caseFile,
	})
	if err != nil {
		return err
	}
	options.say("  %s (%s)", path, time.Since(started).Round(time.Second))
	return nil
}

// discard files a run that never happened, and returns the reason wrapped so a caller can tell it
// from a harness failure. The two are different: a scenario that would not provision says
// something about the scenario, and a harness that fell over says something about the harness.
func (o Options) discard(truth GroundTruthRecord, cause error) error {
	truth.Discarded = &Discarded{Reason: cause.Error(), At: time.Now()}
	if _, err := o.Results.WriteGroundTruth(truth); err != nil {
		return err
	}
	if errors.Is(cause, ErrNotProvisioned) {
		return cause
	}
	return fmt.Errorf("%w: %w", ErrNotProvisioned, cause)
}

// transcriptFor resolves the recording that answers for this scenario's model boundary.
func transcriptFor(scenario Scenario, source ModelSource) (string, error) {
	if source.TranscriptDir == "" {
		return "", fmt.Errorf("scenario %s: %w", scenario.ID, ErrNoLiveProvider)
	}
	return filepath.Join(source.TranscriptDir, scenario.ID+".json"), nil
}

// scenarioRun is everything one scenario needs, all of it real.
type scenarioRun struct {
	truth      *truth
	cluster    *cluster
	terminator *TLSTerminator
	plane      *controlPlane
	relay      *relay
	workDir    string

	connection uuid.UUID
	client     *http.Client
}

func newScenarioRun(
	ctx context.Context, scenario Scenario, transcript string, options Options,
) (*scenarioRun, error) {
	workDir, err := os.MkdirTemp("", "oc-scenario-run")
	if err != nil {
		return nil, fmt.Errorf("creating the run's working directory: %w", err)
	}
	run := &scenarioRun{workDir: workDir, client: &http.Client{Timeout: 30 * time.Second}}

	options.say("  starting a database and a cluster")
	if run.truth, err = startTruth(ctx); err != nil {
		run.close()
		return nil, err
	}
	if run.cluster, err = startCluster(ctx, workDir); err != nil {
		run.close()
		return nil, err
	}

	options.say("  starting the control plane and a relay")
	if err = run.startPlane(ctx, transcript); err != nil {
		run.close()
		return nil, err
	}
	if err = run.startRelay(ctx, scenario); err != nil {
		run.close()
		return nil, err
	}
	return run, nil
}

func (r *scenarioRun) startPlane(ctx context.Context, transcript string) error {
	plane, err := newControlPlane(r.workDir, r.truth.dsn)
	if err != nil {
		return err
	}
	r.plane = plane

	if err = plane.serveOperator(uuid.NewString()); err != nil {
		return err
	}
	plane.replayTranscript(transcript)

	terminator, err := StartTLSTerminator("127.0.0.1", plane.relayAddress)
	if err != nil {
		return fmt.Errorf("starting the tls terminator: %w", err)
	}
	r.terminator = terminator

	if err = plane.start(ctx, terminator.SPKIPin); err != nil {
		return err
	}
	return r.truth.connect(ctx)
}

func (r *scenarioRun) startRelay(ctx context.Context, scenario Scenario) error {
	token, err := r.truth.issueBootstrapToken(ctx, scenarioOrganization)
	if err != nil {
		return fmt.Errorf("issuing a bootstrap token: %w", err)
	}

	installed, err := newRelay(relayInstallation{
		Name: "scenario", WorkDir: r.workDir, Token: token,
		ControlPlaneAddress: r.terminator.Address, SPKIPin: r.terminator.SPKIPin,
		Organization: scenarioOrganization, KubeconfigPath: r.cluster.kubeconfigPath,
		Extra: scenario.RelayEnvironment,
	})
	if err != nil {
		return err
	}
	r.relay = installed
	if err = installed.start(); err != nil {
		return err
	}

	registration, err := r.awaitRegistration(ctx)
	if err != nil {
		return err
	}
	// The Connection is created through the operator API rather than written directly, because
	// that is how a customer's cluster is actually connected and it is part of what a run
	// exercises.
	return r.createConnection(ctx, registration)
}

func (r *scenarioRun) awaitRegistration(ctx context.Context) (uuid.UUID, error) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		id, found, err := r.truth.registration(ctx, scenarioOrganization)
		if found {
			return id, nil
		}
		if time.Now().After(deadline) {
			return uuid.Nil, fmt.Errorf("no relay enrolled within two minutes (last error: %v)\n%s",
				err, r.diagnostics())
		}
		select {
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (r *scenarioRun) createConnection(ctx context.Context, registration uuid.UUID) error {
	var created struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	status, err := r.call(ctx, http.MethodPost,
		fmt.Sprintf("/operator/v1/organizations/%s/environments/default/connections",
			scenarioOrganization),
		map[string]any{
			"integration":         "kubernetes",
			"name":                "scenario cluster",
			"role":                "evidence",
			"locality":            "relay",
			"relayRegistrationId": registration.String(),
		}, &created)
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("creating the evidence connection returned %d", status)
	}

	id, err := uuid.Parse(created.Connection.ID)
	if err != nil {
		return fmt.Errorf("the created connection has no usable identity: %w", err)
	}
	r.connection = id
	return nil
}

// investigated is what the run needs from a finished case beyond the case file itself.
type investigated struct {
	id          string
	versions    versionStamp
	tokens      int64
	microCents  int64
	requests    int
	resultBytes int64
}

type versionStamp struct {
	Model         string `json:"model"`
	PromptVersion string `json:"promptVersion"`
	SchemaVersion string `json:"schemaVersion"`
	Investigator  string `json:"investigator"`
}

// investigate opens a case scoped to the scenario's workload and waits for it to terminate.
func (r *scenarioRun) investigate(ctx context.Context, scenario Scenario) (investigated, error) {
	now := time.Now().UTC()
	var opened struct {
		Investigation struct {
			ID string `json:"id"`
		} `json:"investigation"`
	}
	status, err := r.call(ctx, http.MethodPost,
		fmt.Sprintf("/operator/v1/organizations/%s/investigations", scenarioOrganization),
		map[string]any{
			"connectionId": r.connection.String(),
			"namespace":    scenario.Namespace,
			"workloadKind": scenario.Kind,
			"workloadName": scenario.Workload,
			"windowStart":  now.Add(-scenario.Window).Format(time.RFC3339),
			"windowEnd":    now.Format(time.RFC3339),
		}, &opened)
	if err != nil {
		return investigated{}, err
	}
	if status != http.StatusCreated {
		return investigated{}, fmt.Errorf("opening an investigation returned %d", status)
	}
	return r.awaitTerminal(ctx, opened.Investigation.ID)
}

func (r *scenarioRun) awaitTerminal(ctx context.Context, id string) (investigated, error) {
	deadline := time.Now().Add(investigationDeadline)
	path := fmt.Sprintf("/operator/v1/organizations/%s/investigations/%s",
		scenarioOrganization, id)

	for {
		var summary struct {
			Investigation struct {
				Terminal  bool   `json:"terminal"`
				Lifecycle string `json:"lifecycle"`
			} `json:"investigation"`
			Rounds []struct {
				Versions versionStamp `json:"versions"`
				Spend    struct {
					Tokens      int64 `json:"tokens"`
					MicroCents  int64 `json:"microCents"`
					Requests    int   `json:"requests"`
					ResultBytes int64 `json:"resultBytes"`
				} `json:"spend"`
			} `json:"rounds"`
		}
		status, err := r.call(ctx, http.MethodGet, path, nil, &summary)
		if err != nil {
			return investigated{}, err
		}
		if status != http.StatusOK {
			return investigated{}, fmt.Errorf("reading the investigation returned %d", status)
		}

		if summary.Investigation.Terminal {
			finished := investigated{id: id}
			for _, round := range summary.Rounds {
				finished.versions = round.Versions
				finished.tokens += round.Spend.Tokens
				finished.microCents += round.Spend.MicroCents
				finished.requests += round.Spend.Requests
				finished.resultBytes += round.Spend.ResultBytes
			}
			return finished, nil
		}
		if time.Now().After(deadline) {
			return investigated{}, fmt.Errorf(
				"the investigation was still %s after %s",
				summary.Investigation.Lifecycle, investigationDeadline)
		}
		select {
		case <-ctx.Done():
			return investigated{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// caseFile takes the product's OWN assembly of the case. The same code path serves the shared
// route, both export formats and this artifact, so what a scorer reads cannot diverge from what a
// customer would see.
func (r *scenarioRun) caseFile(ctx context.Context, id string) (json.RawMessage, error) {
	var assembled json.RawMessage
	status, err := r.call(ctx, http.MethodGet,
		fmt.Sprintf("/operator/v1/organizations/%s/investigations/%s/case-file",
			scenarioOrganization, id), nil, &assembled)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("assembling the case file returned %d", status)
	}
	return assembled, nil
}

// call is one operator-API request. Everything the harness does to the product goes through here.
func (r *scenarioRun) call(
	ctx context.Context, method, path string, body any, into any,
) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encoding a %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method,
		"http://"+r.plane.operatorAddress+path, payload)
	if err != nil {
		return 0, fmt.Errorf("building a %s %s: %w", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+r.plane.operatorToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := r.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, fmt.Errorf("reading the answer to %s %s: %w", method, path, err)
	}
	if into != nil && len(raw) > 0 {
		if err = json.Unmarshal(raw, into); err != nil {
			return response.StatusCode, fmt.Errorf("reading the answer to %s %s: %w (%s)",
				method, path, err, truncateForMessage(raw))
		}
	}
	return response.StatusCode, nil
}

// diagnostics renders what each half said, labelled. A failure here has two candidate causes by
// construction, and a message naming neither sends the reader to bisect two codebases.
func (r *scenarioRun) diagnostics() string {
	return fmt.Sprintf("--- control plane ---\n%s\n--- relay ---\n%s",
		r.plane.logs(), r.relay.logs())
}

func (r *scenarioRun) close() {
	if r == nil {
		return
	}
	r.relay.stop()
	r.plane.stop()
	if r.terminator != nil {
		_ = r.terminator.Close()
	}
	r.truth.close()
	r.cluster.close()
	_ = os.RemoveAll(r.workDir)
}

func truncateForMessage(raw []byte) string {
	const ceiling = 512
	if len(raw) <= ceiling {
		return string(raw)
	}
	return string(raw[:ceiling]) + "…"
}
