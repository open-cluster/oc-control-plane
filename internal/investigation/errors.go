package investigation

import "errors"

var (
	ErrUnknown             = errors.New("investigation unknown")
	ErrAlreadyEnded        = errors.New("investigation has already ended")
	ErrIncidentUnknown     = errors.New("incident unknown")
	ErrQueueFull           = errors.New("this organization has too many investigations waiting")
	ErrReasonerUnavailable = errors.New("the reasoning boundary is unavailable")
	ErrBadCursor           = errors.New("after is not a page position from a previous response")
)
