package investigation

import "time"

// AssignedMessage is canonical person-authored input for one Investigation.
type AssignedMessage struct {
	Sequence  int64     `json:"sequence"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"createdAt"`
	Text      string    `json:"text"`
}
