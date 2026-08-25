package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"
)

type alertFixture struct {
	Name   string            `json:"alertname"`
	Labels map[string]string `json:"labels"`
}

// Cases returns every executable scenario with operational timestamps relative to now.
func Cases(now time.Time) []Case {
	cases, err := LoadCases(now)
	if err != nil {
		panic("loading evaluation scenarios: " + err.Error())
	}
	return cases
}

// LoadCases composes independent alert, cluster, GitHub, Slack, and truth fixtures.
func LoadCases(now time.Time) ([]Case, error) {
	catalog, err := Load()
	if err != nil {
		return nil, err
	}
	cases := make([]Case, 0, len(catalog.Fixtures))
	for _, fixture := range catalog.Fixtures {
		metadata, err := loadMetadata(fixture.Name)
		if err != nil {
			return nil, err
		}
		one := Case{
			Name: fixture.Name, Revision: catalog.Revision + "/" + fixture.Revision,
			Safety: fixture.Safety, Truth: fixture.GroundTruth,
			Question: metadata.Question, FollowUps: metadata.FollowUps,
			DistractorSlackToken:   metadata.DistractorSlackToken,
			DistractorInstallation: metadata.DistractorInstallation,
			FailCommits:            metadata.FailCommits, MoreHistory: metadata.MoreHistory,
			MoreCommits: metadata.MoreCommits,
		}
		var alert alertFixture
		if err := decodeOptionalJSON(fixture.Name, "alert.json", &alert); err != nil {
			return nil, err
		}
		one.Alertname, one.Labels = alert.Name, alert.Labels
		for _, source := range []struct {
			name        string
			destination any
		}{
			{name: "kubernetes.json", destination: &one.Kubernetes},
			{name: "github.json", destination: &one.Installations},
			{name: "slack.json", destination: &one.Workspaces},
		} {
			if err := decodeOptionalJSON(fixture.Name, source.name, source.destination); err != nil {
				return nil, err
			}
		}
		rebaseOperationalTimes(&one, now.Sub(metadata.ReferenceTime))
		cases = append(cases, one)
	}
	return cases, nil
}

func decodeOptionalJSON(scenario, name string, destination any) error {
	filename := path.Join("cases", scenario, name)
	raw, err := fixtureFiles.ReadFile(filename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading evaluation scenario %s: %w", filename, err)
	}
	if err := json.Unmarshal(raw, destination); err != nil {
		return fmt.Errorf("decoding evaluation scenario %s: %w", filename, err)
	}
	return nil
}

func rebaseOperationalTimes(scenario *Case, offset time.Duration) {
	for token, workspace := range scenario.Workspaces {
		for channelIndex := range workspace.Channels {
			for messageIndex := range workspace.Channels[channelIndex].Messages {
				message := &workspace.Channels[channelIndex].Messages[messageIndex]
				if !message.At.IsZero() {
					message.At = message.At.Add(offset)
				}
			}
		}
		scenario.Workspaces[token] = workspace
	}
	for identifier, installation := range scenario.Installations {
		for repositoryIndex := range installation.Repos {
			repository := &installation.Repos[repositoryIndex]
			for commitIndex := range repository.Commits {
				if !repository.Commits[commitIndex].At.IsZero() {
					repository.Commits[commitIndex].At = repository.Commits[commitIndex].At.Add(offset)
				}
			}
			for pullIndex := range repository.Pulls {
				if !repository.Pulls[pullIndex].MergedAt.IsZero() {
					repository.Pulls[pullIndex].MergedAt = repository.Pulls[pullIndex].MergedAt.Add(offset)
				}
			}
		}
		scenario.Installations[identifier] = installation
	}
}
