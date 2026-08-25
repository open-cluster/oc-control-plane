package eval

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"time"

	"gopkg.in/yaml.v3"
)

// ScorerRevision identifies the deterministic structural scoring contract.
const ScorerRevision = "structural-v1"

//go:embed cases/*/*.yaml cases/*/*.json
var fixtureFiles embed.FS

// Cause describes a conclusion that must be supported by an observed tool read.
type Cause struct {
	Name    string   `json:"name"`
	Markers []string `json:"markers"`
	Tools   []string `json:"tools"`
}

// Read describes evidence that separates competing explanations.
type Read struct {
	Tool      string `json:"tool"`
	ArgMarker string `json:"argMarker,omitempty"`
}

// GroundTruth is the independently authored structural scoring expectation.
type GroundTruth struct {
	Causes         []Cause  `json:"causes,omitempty"`
	MustNotClaim   []string `json:"mustNotClaim,omitempty"`
	Discriminating []Read   `json:"discriminating,omitempty"`
	RelevantTools  []string `json:"relevantTools,omitempty"`
	ExpectFindings bool     `json:"expectFindings"`
	MustNotAnswer  []string `json:"mustNotAnswer,omitempty"`
	AnswerMarkers  []string `json:"answerMarkers,omitempty"`
	Survives       []string `json:"survives,omitempty"`
}

// World describes the observable operational situation independently of composition.
type World struct {
	Template    string   `json:"template"`
	Situation   string   `json:"situation"`
	Distractors []string `json:"distractors,omitempty"`
}

// Safety captures assertions that remain mandatory for every model and prompt revision.
type Safety struct {
	RequireCitations    bool `json:"requireCitations"`
	RejectSecretLeakage bool `json:"rejectSecretLeakage"`
	HonestInsufficiency bool `json:"honestInsufficiency,omitempty"`
}

// Fixture is one versioned investigation or follow-up evaluation world.
type Fixture struct {
	Name        string      `json:"name"`
	Revision    string      `json:"revision"`
	World       World       `json:"world"`
	GroundTruth GroundTruth `json:"groundTruth"`
	Safety      Safety      `json:"safety"`
}

// Catalog contains all versioned evaluation worlds.
type Catalog struct {
	Revision string    `json:"revision"`
	Fixtures []Fixture `json:"fixtures"`
}

type scenarioMetadata struct {
	Name                   string    `yaml:"name"`
	Revision               string    `yaml:"revision"`
	Situation              string    `yaml:"situation"`
	ReferenceTime          time.Time `yaml:"referenceTime"`
	Safety                 Safety    `yaml:"safety"`
	Question               string    `yaml:"question"`
	FollowUps              []string  `yaml:"followUps"`
	Distractors            []string  `yaml:"distractors"`
	DistractorSlackToken   string    `yaml:"distractorSlackToken"`
	DistractorInstallation string    `yaml:"distractorInstallation"`
	FailCommits            int       `yaml:"failCommits"`
	MoreHistory            bool      `yaml:"moreHistory"`
	MoreCommits            bool      `yaml:"moreCommits"`
}

// Load reads independently authored scenario descriptions and expected outcomes.
func Load() (Catalog, error) {
	scenarios, err := fs.ReadDir(fixtureFiles, "cases")
	if err != nil {
		return Catalog{}, fmt.Errorf("reading evaluation scenarios: %w", err)
	}
	catalog := Catalog{Fixtures: make([]Fixture, 0, len(scenarios))}
	for _, scenario := range scenarios {
		if !scenario.IsDir() {
			continue
		}
		metadata, err := loadMetadata(scenario.Name())
		if err != nil {
			return Catalog{}, err
		}
		if metadata.Name != scenario.Name() {
			return Catalog{}, fmt.Errorf("evaluation scenario %q declares a different name %q", scenario.Name(), metadata.Name)
		}
		if metadata.ReferenceTime.IsZero() {
			return Catalog{}, fmt.Errorf("evaluation scenario %q requires a reference time", scenario.Name())
		}
		revision := metadata.ReferenceTime.UTC().Format("2006-01-02") + ".1"
		if catalog.Revision == "" {
			catalog.Revision = revision
		} else if catalog.Revision != revision {
			return Catalog{}, fmt.Errorf("evaluation scenario %q uses a different catalog revision", scenario.Name())
		}
		var truth GroundTruth
		if err := decodeYAML(path.Join("cases", scenario.Name(), "truth.yaml"), &truth); err != nil {
			return Catalog{}, err
		}
		catalog.Fixtures = append(catalog.Fixtures, Fixture{
			Name: metadata.Name, Revision: metadata.Revision,
			World:       World{Template: metadata.Name, Situation: metadata.Situation, Distractors: metadata.Distractors},
			GroundTruth: truth, Safety: metadata.Safety,
		})
	}
	if catalog.Revision == "" || len(catalog.Fixtures) == 0 {
		return Catalog{}, fmt.Errorf("evaluation fixtures require a revision and at least one world")
	}
	seen := make(map[string]bool, len(catalog.Fixtures))
	for _, fixture := range catalog.Fixtures {
		if fixture.Name == "" || fixture.Revision == "" || fixture.World.Template == "" ||
			fixture.World.Situation == "" {
			return Catalog{}, fmt.Errorf("evaluation fixture %q requires a name, revision, world template, and situation", fixture.Name)
		}
		if !fixture.Safety.RequireCitations || !fixture.Safety.RejectSecretLeakage {
			return Catalog{}, fmt.Errorf("evaluation fixture %q must require citations and reject secret leakage", fixture.Name)
		}
		if seen[fixture.Name] {
			return Catalog{}, fmt.Errorf("duplicate evaluation fixture %q", fixture.Name)
		}
		seen[fixture.Name] = true
	}
	return catalog, nil
}

func loadMetadata(name string) (scenarioMetadata, error) {
	var metadata scenarioMetadata
	if err := decodeYAML(path.Join("cases", name, "case.yaml"), &metadata); err != nil {
		return scenarioMetadata{}, err
	}
	return metadata, nil
}

func decodeYAML(name string, destination any) error {
	raw, err := fixtureFiles.ReadFile(name)
	if err != nil {
		return fmt.Errorf("reading evaluation scenario %s: %w", name, err)
	}
	if err := yaml.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("decoding evaluation scenario %s: %w", name, err)
	}
	return nil
}

// Lookup finds the independently versioned fixture for a named world.
func (c Catalog) Lookup(name string) (Fixture, bool) {
	for _, fixture := range c.Fixtures {
		if fixture.Name == name {
			return fixture, true
		}
	}
	return Fixture{}, false
}
