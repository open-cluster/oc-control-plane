package incident

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrUnknown   = errors.New("incident unknown")
	ErrMerge     = errors.New("these incidents cannot be merged")
	ErrBadCursor = errors.New("cursor is not a page position")
)

type Status int16

const (
	StatusOpen Status = iota + 1
	StatusResolved
)

func (s Status) String() string {
	switch s {
	case StatusOpen:
		return "open"
	case StatusResolved:
		return "resolved"
	default:
		return "unrecognised"
	}
}

func ParseStatus(value string) (Status, bool) {
	switch value {
	case "open":
		return StatusOpen, true
	case "resolved":
		return StatusResolved, true
	default:
		return 0, false
	}
}

type Basis int16

const (
	BasisSourceGrouping Basis = iota + 1
	BasisUngrouped
)

func (b Basis) String() string {
	switch b {
	case BasisSourceGrouping:
		return "source_grouping"
	case BasisUngrouped:
		return "ungrouped"
	default:
		return "unrecognised"
	}
}

func (b Basis) Explain() string {
	switch b {
	case BasisSourceGrouping:
		return "the source that delivered these alerts grouped them under one identity of its own"
	case BasisUngrouped:
		return "the source supplied no grouping identity, so this alert is an incident by itself"
	default:
		return "the basis for this grouping was not recorded"
	}
}

type Incident struct {
	ID              uuid.UUID
	Organization    string
	Integration     uuid.UUID
	IntegrationName string
	GroupingKey     string
	Basis           Basis
	Title           string
	Status          Status
	FirstSeenAt     time.Time
	LastSeenAt      time.Time
	ResolvedAt      time.Time
	AlertEventCount int
	SupersededBy    *uuid.UUID
	SupersededAt    time.Time
	SupersedeReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (e Incident) Superseded() bool { return e.SupersededBy != nil }

type AlertEvent struct {
	ID         uuid.UUID
	Title      string
	Summary    string
	Labels     map[string]string
	Firing     bool
	StartedAt  time.Time
	ResolvedAt time.Time
	ReceivedAt time.Time
}

type Merge struct {
	Absorbed uuid.UUID
	Into     uuid.UUID
	Reason   string
}

func (m Merge) Validate() error {
	switch {
	case m.Absorbed == uuid.Nil || m.Into == uuid.Nil:
		return errors.New("a merge names two incidents")
	case m.Absorbed == m.Into:
		return errors.New("an incident cannot be merged into itself")
	case len(m.Reason) == 0:
		return errors.New("a merge states why these are one incident")
	case len(m.Reason) > MaxReasonLength:
		return errors.New("the reason is longer than this record holds")
	default:
		return nil
	}
}

const MaxReasonLength = 1024
