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
//
// It must exceed the round's OWN deadline, or the harness abandons investigations the product
// would have finished and files them as failures of the product rather than of the instrument. The
// round is bounded at forty-five minutes; this is the harness waiting slightly longer than that so
// that whichever bound fires, it is the one whose expiry means something.
const investigationDeadline = 50 * time.Minute

// scenarioOrganization is the tenant every scenario run belongs to. One is enough: tenant
// isolation is proven in the control plane's own suite, and nothing about an explanation varies
// by tenant.
const scenarioOrganization = "scenario-harness"

// ErrNoModelSource reports a run that was given nothing to reason with.
//
// The specification says the harness calls a real provider and records the transcript as a
// by-product. A run must therefore be given either a live deployment or a directory of recordings;
// with neither it says so out loud rather than quietly scoring something that was never asked of a
// model.
var ErrNoModelSource = errors.New(
	"this run was given no model source: name a live provider with -provider, -model and " +
		"-key-file, or a directory of recordings with -transcripts")

// ModelSource is where a run's reasoning comes from.
//
// A live deployment outranks a recording, because a run given both was asked for the real thing
// and replaying at it would answer a different question.
type ModelSource struct {
	// TranscriptDir holds one recording per scenario, named <scenario-id>.json.
	TranscriptDir string

	// The live deployment. The credential is a PATH: it reaches the control plane as an
	// environment variable naming a file, never as the key itself, because that process's
	// environment is readable from a process listing.
	Provider string
	Model    string
	KeyFile  string
	Effort   string
}

// Live reports whether a real provider was configured.
func (m ModelSource) Live() bool {
	return m.Provider != "" && m.Model != "" && m.KeyFile != ""
}

// Describe names the source for the artifact, so a scorer reading a replayed run knows they are
// reading a reproduction — and one reading a live run knows exactly which model answered.
func (m ModelSource) Describe() string {
	if m.Live() {
		return "live provider " + m.Provider + "/" + m.Model
	}
	if m.TranscriptDir != "" {
		return "recorded transcript"
	}
	return "none"
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
//
// A live deployment needs none, and says so by returning an empty path rather than an error: the
// control plane is then configured with a provider instead, and the recording this run produces is
// a by-product rather than an input.
func transcriptFor(scenario Scenario, source ModelSource) (string, error) {
	if source.Live() {
		return "", nil
	}
	if source.TranscriptDir == "" {
		return "", fmt.Errorf("scenario %s: %w", scenario.ID, ErrNoModelSource)
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
	// transcripts is where a live run files what the model said. It is under the results rather
	// than under the run's working directory, which is removed when the run closes — a recording
	// that vanished with the run would answer nothing about a scenario that failed.
	transcripts string
}

func newScenarioRun(
	ctx context.Context, scenario Scenario, transcript string, options Options,
) (*scenarioRun, error) {
	workDir, err := os.MkdirTemp("", "oc-scenario-run")
	if err != nil {
		return nil, fmt.Errorf("creating the run's working directory: %w", err)
	}
	run := &scenarioRun{
		workDir:     workDir,
		client:      &http.Client{Timeout: 30 * time.Second},
		transcripts: options.Results.TranscriptDir(),
	}

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
	if err = run.startPlane(ctx, transcript, options.Model); err != nil {
		run.close()
		return nil, err
	}
	if err = run.startRelay(ctx, scenario); err != nil {
		run.close()
		return nil, err
	}
	return run, nil
}

func (r *scenarioRun) startPlane(
	ctx context.Context, transcript string, source ModelSource,
) error {
	plane, err := newControlPlane(r.workDir, r.truth.dsn)
	if err != nil {
		return err
	}
	r.plane = plane

	if err = plane.serveOperator(uuid.NewString()); err != nil {
		return err
	}
	// A live deployment outranks a recording: a run given one was asked for the real thing.
	if source.Live() {
		plane.useModel(source.Provider, source.Model, source.KeyFile, source.Effort)
		// And it records what it says. A replayed run does not, because re-recording a recording
		// produces a copy of the file being replayed.
		plane.recordTranscripts(r.transcripts)
	} else {
		plane.replayTranscript(transcript)
	}

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

// defaultEnvironment resolves the Environment every scenario's Connection is created in.
//
// It is RESOLVED rather than assumed. An Environment is addressed by identity, and the harness
// used to spell a name into the path — which the operator surface refuses, because a name can be
// changed and an identity cannot. Reading the list is also what creates the Default for an
// organization that has never been seen before, which is what a scenario run always is.
func (r *scenarioRun) defaultEnvironment(ctx context.Context) (uuid.UUID, error) {
	var listed struct {
		Environments []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			IsDefault bool   `json:"isDefault"`
		} `json:"environments"`
	}
	status, raw, err := r.call(ctx, http.MethodGet,
		fmt.Sprintf("/operator/v1/organizations/%s/environments", scenarioOrganization),
		nil, &listed)
	if err != nil {
		return uuid.Nil, err
	}
	if status != http.StatusOK {
		return uuid.Nil, fmt.Errorf("listing environments returned %d: %s",
			status, truncateForMessage(raw))
	}

	for _, environment := range listed.Environments {
		// The Default is taken from the flag rather than from the name, because the flag is the
		// fact and the name is only what somebody called it.
		if !environment.IsDefault {
			continue
		}
		id, parseErr := uuid.Parse(environment.ID)
		if parseErr != nil {
			return uuid.Nil, fmt.Errorf("the default environment has no usable identity: %w",
				parseErr)
		}
		return id, nil
	}
	return uuid.Nil, fmt.Errorf(
		"this organization has no default environment; %d were listed", len(listed.Environments))
}

func (r *scenarioRun) createConnection(ctx context.Context, registration uuid.UUID) error {
	environment, err := r.defaultEnvironment(ctx)
	if err != nil {
		return err
	}

	var created struct {
		Connection struct {
			ID string `json:"id"`
		} `json:"connection"`
	}
	status, raw, err := r.call(ctx, http.MethodPost,
		fmt.Sprintf("/operator/v1/organizations/%s/environments/%s/connections",
			scenarioOrganization, environment),
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
		return fmt.Errorf("creating the evidence connection returned %d: %s",
			status, truncateForMessage(raw))
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
	status, raw, err := r.call(ctx, http.MethodPost,
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
		return investigated{}, fmt.Errorf("opening an investigation returned %d: %s",
			status, truncateForMessage(raw))
	}
	return r.awaitTerminal(ctx, opened.Investigation.ID)
}

func (r *scenarioRun) awaitTerminal(ctx context.Context, id string) (investigated, error) {
	deadline := time.Now().Add(investigationDeadline)
	path := fmt.Sprintf("/operator/v1/organizations/%s/investigations/%s",
		scenarioOrganization, id)

	for {
		// This is the SUMMARY's shape, and getting it wrong is silent: an array that does not
		// exist decodes to nothing, and the artifact then reports a run that cost nothing and was
		// produced by no model. The summary carries the current round, the case's accumulated
		// spend, and counts — not a rounds array.
		var summary struct {
			Investigation struct {
				Terminal  bool   `json:"terminal"`
				Lifecycle string `json:"lifecycle"`
			} `json:"investigation"`
			CurrentRound *struct {
				Versions versionStamp `json:"versions"`
			} `json:"currentRound"`
			Counts struct {
				Requests int `json:"activity"`
			} `json:"counts"`
			Spend struct {
				Tokens     int64 `json:"tokens"`
				MicroCents int64 `json:"microCents"`
			} `json:"spend"`
		}
		status, _, err := r.call(ctx, http.MethodGet, path, nil, &summary)
		if err != nil {
			return investigated{}, err
		}
		if status != http.StatusOK {
			return investigated{}, fmt.Errorf("reading the investigation returned %d", status)
		}

		if summary.Investigation.Terminal {
			finished := investigated{
				id:         id,
				tokens:     summary.Spend.Tokens,
				microCents: summary.Spend.MicroCents,
				requests:   summary.Counts.Requests,
			}
			if summary.CurrentRound != nil {
				finished.versions = summary.CurrentRound.Versions
			}
			// The components are what a blind scorer needs most: an artifact that cannot say
			// which model produced the explanation cannot be compared to any other run. If the
			// summary no longer carries them, say so here rather than filing an empty field that
			// reads as though nothing answered.
			if finished.versions.Model == "" {
				return investigated{}, fmt.Errorf(
					"the investigation finished but the summary named no model; the artifact " +
						"would be unattributable")
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
	status, _, err := r.call(ctx, http.MethodGet,
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
// call performs one operator request and hands back the raw body as well as the status.
//
// The body travels because a failing status without one is undiagnosable: a 400 says the request
// was refused and the reason is the only part worth reading. The harness provisions a cluster and
// two processes before it gets here, so a message that sends the reader back to reproduce it by
// hand costs minutes every time.
func (r *scenarioRun) call(
	ctx context.Context, method, path string, body any, into any,
) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding a %s %s: %w", method, path, err)
		}
		payload = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method,
		"http://"+r.plane.operatorAddress+path, payload)
	if err != nil {
		return 0, nil, fmt.Errorf("building a %s %s: %w", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+r.plane.operatorToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := r.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, nil, fmt.Errorf(
			"reading the answer to %s %s: %w", method, path, err)
	}
	// Only a success is decoded. A refusal's body is an error document rather than the shape the
	// caller asked for, and failing to decode it would replace the reason with a parse error.
	if into != nil && len(raw) > 0 && response.StatusCode < 300 {
		if err = json.Unmarshal(raw, into); err != nil {
			return response.StatusCode, raw, fmt.Errorf(
				"reading the answer to %s %s: %w (%s)",
				method, path, err, truncateForMessage(raw))
		}
	}
	return response.StatusCode, raw, nil
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
