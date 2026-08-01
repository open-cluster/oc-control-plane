package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// What a run produces: one file a scorer reads, and one file they must not.
//
// The separation is the instrument's central discipline. A scorer who knows the declared cause
// grades recognition rather than reasoning, so ground truth is written to a different file, keyed
// by an opaque run identifier, and joined only after the scoring is recorded.
//
// Nothing in the scored artifact names the scenario. Not its identifier, not its title, not its
// namespace — "scenario-image-pull" in a brief would give the answer away as surely as printing
// the cause would, which is why the scenarios carry ordinary team namespaces instead.

// artifactSchema is stamped into every artifact. A scorer's spreadsheet and a comparison between
// two runs both read this, and a change to what an artifact contains has to be visible rather
// than inferred.
const artifactSchema = 1

// Artifact is what an evaluating engineer reads. It carries no ground truth.
//
// It is deliberately readable without access to the codebase: the judgement being asked for is
// about the output, and a scorer who has to read the implementation to understand the artifact is
// being asked a different question.
type Artifact struct {
	Schema int    `json:"schema"`
	RunID  string `json:"runId"`
	// Components are what produced this run. A regression has to be attributable, and a model
	// upgrade that degraded investigations is the specific failure a customer would otherwise
	// report first.
	Components Components `json:"components"`
	// Cost and Elapsed are the two numbers the buyer actually asks about: what it costs, and
	// whether waiting beats typing.
	Cost    Cost          `json:"cost"`
	Elapsed time.Duration `json:"elapsedNanoseconds"`
	// ElapsedHuman is the same figure a person can read without dividing by a billion.
	ElapsedHuman string `json:"elapsed"`
	// CaseFile is the case as the product itself assembles it — the same bytes a share, an export
	// and this artifact all take, so a scored artifact cannot diverge from what a customer sees.
	// It carries the brief, every request with its justification, every EvidenceItem and
	// CoverageGap, the timeline, the hypotheses and the terminal outcome.
	CaseFile json.RawMessage `json:"caseFile"`
	// Assumptions is what this instrument does not prove about itself. It travels IN the artifact
	// rather than in a document nobody opens, because the person scoring it is the person who has
	// to know the limits of what their score means.
	Assumptions []string `json:"assumptions"`
}

// Components identify everything whose change would change the answer.
type Components struct {
	CodeVersion   string `json:"codeVersion"`
	Model         string `json:"model"`
	PromptVersion string `json:"promptVersion"`
	SchemaVersion string `json:"schemaVersion"`
	Investigator  string `json:"investigator"`
	// ModelSource says whether a live provider answered or a recording did. A scorer reading a
	// replayed run is reading a reproduction, and the artifact says so rather than letting the
	// distinction be lost.
	ModelSource string `json:"modelSource"`
}

// Cost is what the run consumed, so the feature can be priced.
type Cost struct {
	Tokens     int64 `json:"tokens"`
	MicroCents int64 `json:"microCents"`
	Requests   int   `json:"capabilityRequests"`
	ResultKiB  int64 `json:"resultKiB"`
}

// standingAssumptions is what every artifact states about the instrument that produced it.
//
// The first is the one that matters. These are ten failures chosen by the person building the
// system, which measures whether it works on failures he already understands; it is stated rather
// than engineered away, because pretending otherwise would make the score mean more than it does.
var standingAssumptions = []string{
	"The scenario set is not representative and cannot be made so. It is ten failures chosen by " +
		"the person building the system, which measures whether it works on failures he already " +
		"understands. Real incidents from a design partner are what will change that.",
	"Every scenario is synthetic and contains no real data. Nothing here was observed in a " +
		"production system.",
	"The cluster reached the scenario's declared broken state before this run was scored. A run " +
		"whose cluster did not is discarded and never reaches a scorer.",
}

// GroundTruthRecord is the answer, filed apart from the artifact and keyed to it by run.
//
// It is a separate FILE rather than a separate field, because a field is one careless render away
// from being shown to the person whose whole value is not having seen it.
type GroundTruthRecord struct {
	Schema     int         `json:"schema"`
	RunID      string      `json:"runId"`
	ScenarioID string      `json:"scenarioId"`
	Title      string      `json:"title"`
	Cause      string      `json:"cause"`
	Decisive   []string    `json:"decisiveEvidence"`
	Expected   string      `json:"expectedOutcome"`
	Note       string      `json:"whyThisScenarioIsInTheSet"`
	RecordedAt time.Time   `json:"recordedAt"`
	Discarded  *Discarded  `json:"discarded,omitempty"`
	Components *Components `json:"components,omitempty"`
}

// Discarded records a run that did not happen and why. A discarded run is filed rather than
// dropped: the fact that a scenario could not be provisioned is itself information about the
// instrument, and silently missing runs would make a set look complete when it was not.
type Discarded struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// GroundTruthOf renders a scenario's declared truth for a run.
func GroundTruthOf(scenario Scenario, runID string, at time.Time) GroundTruthRecord {
	return GroundTruthRecord{
		Schema: artifactSchema, RunID: runID, ScenarioID: scenario.ID, Title: scenario.Title,
		Cause: scenario.Truth.Cause, Decisive: scenario.Truth.Decisive,
		Expected: scenario.Truth.Expected.String(), Note: scenario.Truth.Note,
		RecordedAt: at,
	}
}

// Results is where a run's files are written: artifacts a scorer opens, and ground truth they do
// not. Two directories rather than two filename patterns, so "give the scorers the artifacts"
// is handing over a directory rather than a selection anyone can get wrong.
type Results struct {
	root string
}

// NewResults prepares the output directories.
func NewResults(root string) (*Results, error) {
	for _, directory := range []string{
		filepath.Join(root, "artifacts"),
		filepath.Join(root, "ground-truth"),
		filepath.Join(root, "scores"),
	} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return nil, fmt.Errorf("preparing %s: %w", directory, err)
		}
	}
	return &Results{root: root}, nil
}

// ArtifactDir is the directory to hand a scorer. Nothing in it names a scenario or its cause.
func (r *Results) ArtifactDir() string { return filepath.Join(r.root, "artifacts") }

// WriteArtifact files what a scorer reads.
func (r *Results) WriteArtifact(artifact Artifact) (string, error) {
	artifact.Schema = artifactSchema
	artifact.Assumptions = standingAssumptions
	artifact.ElapsedHuman = artifact.Elapsed.Round(time.Millisecond).String()
	return r.write(filepath.Join("artifacts", artifact.RunID+".json"), artifact)
}

// WriteGroundTruth files the answer, apart.
func (r *Results) WriteGroundTruth(record GroundTruthRecord) (string, error) {
	return r.write(filepath.Join("ground-truth", record.RunID+".json"), record)
}

// WriteSetResult files the joined result of a scored set.
func (r *Results) WriteSetResult(result SetResult) (string, error) {
	return r.write(filepath.Join("scores", "set-result.json"), result)
}

// ReadGroundTruth loads every declared truth filed here.
func (r *Results) ReadGroundTruth() ([]GroundTruthRecord, error) {
	return filedIn[GroundTruthRecord](filepath.Join(r.root, "ground-truth"), nil)
}

// ReadScores loads every recorded score. Scores are written by hand or by a form; this only reads
// them, because a scoring tool that could also write a score is one that could write one nobody
// gave.
func (r *Results) ReadScores() ([]Score, error) {
	// The joined result is filed beside the scores and is not one, so it is skipped by name. A
	// directory of its own would be tidier and would also be a fourth place to look for a run.
	return filedIn[Score](filepath.Join(r.root, "scores"), map[string]bool{"set-result.json": true})
}

// filedIn decodes every JSON document in a directory. One reader serves both kinds because the
// only thing that differs between them is what they decode into, and two copies of a
// read-dir-filter-decode loop is two places for the filter to drift.
func filedIn[T any](directory string, skip map[string]bool) ([]T, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", directory, err)
	}
	found := make([]T, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || skip[entry.Name()] {
			continue
		}
		var decoded T
		if err = readJSON(filepath.Join(directory, entry.Name()), &decoded); err != nil {
			return nil, err
		}
		found = append(found, decoded)
	}
	return found, nil
}

func (r *Results) write(relative string, value any) (string, error) {
	path := filepath.Join(r.root, relative)
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding %s: %w", relative, err)
	}
	if err = os.WriteFile(path, append(encoded, '\n'), 0o640); err != nil {
		return "", fmt.Errorf("writing %s: %w", relative, err)
	}
	return path, nil
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err = json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	return nil
}
