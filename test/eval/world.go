package eval

import "time"

const (
	PrimaryToken        = "xoxb-eval-primary"
	PrimaryInstallation = "5001"
)

type Message struct {
	At         time.Time
	User       string
	Text       string
	ThreadTS   string
	ReplyCount int
}

type Channel struct {
	ID       string
	Name     string
	Topic    string
	Messages []Message
}

type Workspace struct {
	Team     string
	Channels []Channel
	Users    map[string]string
}

type Commit struct {
	SHA     string
	Message string
	Author  string
	At      time.Time
	Files   []Change
}

type Change struct {
	Path  string
	Patch string
}

type Pull struct {
	Number   int
	Title    string
	State    string
	MergedAt time.Time
	Author   string
}

type Repository struct {
	ID      int64
	Name    string
	Commits []Commit
	Pulls   []Pull
	Files   map[string]string
}

type Installation struct {
	Account string
	Repos   []Repository
}

type Workload struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type Kubernetes struct {
	Workloads []Workload `json:"workloads"`
}

type Case struct {
	Name                   string
	Revision               string
	Safety                 Safety
	Alertname              string
	Labels                 map[string]string
	Kubernetes             Kubernetes
	Workspaces             map[string]Workspace
	Installations          map[string]Installation
	DistractorSlackToken   string
	DistractorInstallation string
	FailCommits            int
	MoreHistory            bool
	MoreCommits            bool
	Question               string
	FollowUps              []string
	Truth                  GroundTruth
}
