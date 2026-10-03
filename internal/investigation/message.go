package investigation

import "time"

type AssignedMessage struct {
	Sequence  int64     `json:"sequence"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"createdAt"`
	Text      string    `json:"text"`
}
