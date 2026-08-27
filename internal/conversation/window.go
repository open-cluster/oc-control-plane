package conversation

import "time"

// MinimumQuestionWindow is how far back a turn with no incident reaches.
//
// A turn attached to an incident anchors its window to when that incident began, and the
// investigation lead widens it backwards so the change that caused it sits inside the
// window every read is clamped to. A turn with no incident has no onset to anchor to, so
// there is nothing for a lead to lead. Borrowing it as a total width answered "what
// changed recently?" against the last two hours and called an untouched window an
// untouched estate.
//
// A day is what "recently" means to somebody asking about production, and it is
// deliberately NOT configurable: a question with no incident carries no operator
// judgement to encode, and a conversation that needs a different window should be
// attached to an incident, which is exactly what an incident is for.
const MinimumQuestionWindow = 24 * time.Hour

// QuestionWindow is how far back a turn reaches when it is about no particular incident.
// A deployment that widened the investigation lead meant "look further back", so a lead
// past the floor is honoured; a shorter, absent or nonsensical one cannot narrow a
// question below the floor, because a collapsed window makes every read empty and every
// answer a false negative.
func QuestionWindow(lead time.Duration) time.Duration {
	if lead > MinimumQuestionWindow {
		return lead
	}
	return MinimumQuestionWindow
}
