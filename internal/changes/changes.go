package changes

import (
	"time"

	"github.com/google/uuid"
)

type ObjectKind int

const (
	KindDeployment  ObjectKind = 1
	KindStatefulSet ObjectKind = 2
	KindDaemonSet   ObjectKind = 3
	KindConfigMap   ObjectKind = 4
	KindSecret      ObjectKind = 5
)

func (k ObjectKind) String() string {
	switch k {
	case KindDeployment:
		return "deployment"
	case KindStatefulSet:
		return "statefulset"
	case KindDaemonSet:
		return "daemonset"
	case KindConfigMap:
		return "configmap"
	case KindSecret:
		return "secret"
	default:
		return "unknown"
	}
}

type ChangeKind int

const (
	ChangeBaseline ChangeKind = 1
	ChangeCreated  ChangeKind = 2
	ChangeModified ChangeKind = 3
	ChangeDeleted  ChangeKind = 4
)

func (k ChangeKind) String() string {
	switch k {
	case ChangeBaseline:
		return "baseline"
	case ChangeCreated:
		return "created"
	case ChangeModified:
		return "modified"
	case ChangeDeleted:
		return "deleted"
	default:
		return "unknown"
	}
}

type FieldChange struct {
	Field  string `json:"field"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

type Change struct {
	Namespace        string
	Kind             ObjectKind
	Name             string
	UID              string
	ObservedRevision string
	Change           ChangeKind
	Fields           []FieldChange
}

type Delta struct {
	IntegrationID  uuid.UUID
	PolicyRevision int64
	Baseline       bool
	ObservedAt     time.Time
	Changes        []Change
}

type Event struct {
	ID               int64
	IntegrationID    uuid.UUID
	Namespace        string
	Kind             ObjectKind
	Name             string
	UID              string
	ObservedRevision string
	Change           ChangeKind
	ObservedAt       time.Time
	RecordedAt       time.Time
	Fields           []FieldChange
}

func (e Event) Summary() string {
	subject := e.Kind.String() + " " + e.Name
	switch e.Change {
	case ChangeCreated:
		return subject + " was created"
	case ChangeDeleted:
		return subject + " was deleted"
	case ChangeModified:
		if detail := summarizeFields(e.Fields); detail != "" {
			return subject + ": " + detail
		}
		return subject + " changed"
	default:
		return subject + " was first observed"
	}
}

func summarizeFields(fields []FieldChange) string {
	const most = 3
	rendered := ""
	for i, field := range fields {
		if i == most {
			return rendered + ", and more"
		}
		if i > 0 {
			rendered += ", "
		}
		switch {
		case field.Before == "":
			rendered += field.Field + " set to " + field.After
		case field.After == "":
			rendered += field.Field + " removed (was " + field.Before + ")"
		default:
			rendered += field.Field + " " + field.Before + " -> " + field.After
		}
	}
	return rendered
}

type Scope struct {
	IntegrationID     uuid.UUID
	PolicyRevision    int64
	RequestedInterval time.Duration
	CoveredSince      *time.Time
	BaselineAt        *time.Time
	LastConfirmedAt   *time.Time
	Faulted           bool
	Truncated         bool
}

type Freshness struct {
	IntegrationID  uuid.UUID
	PolicyRevision int64
	CompletedAt    *time.Time
	Faulted        bool
	Truncated      bool
}

type Recorded struct {
	Inserted int
	Refused  bool
}

type WindowChanges struct {
	Covered   bool
	Scope     Scope
	Events    []Event
	Truncated bool
}
