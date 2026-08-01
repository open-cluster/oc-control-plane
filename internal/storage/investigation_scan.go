package storage

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
)

// How a row becomes one of the capability's values.
//
// One mapping function serves every read of each table, which is what makes a column added to one
// query and forgotten in another a compile error rather than a field that is silently always
// zero. The nullable columns are read through pointers and flattened, because a caller asking
// whether an item is on the timeline should not have to dereference to find out.

func scanHypothesis(row scanned) (investigation.Hypothesis, error) {
	var (
		hypothesis investigation.Hypothesis
		state      int16
	)
	if err := row.Scan(&hypothesis.ID, new(uuid.UUID), &hypothesis.RoundID, &hypothesis.Ordinal,
		&hypothesis.Statement, &hypothesis.Falsifies, &state, &hypothesis.SetAsideReason,
		&hypothesis.ProposedAt, &hypothesis.UpdatedAt); err != nil {
		return investigation.Hypothesis{}, err
	}
	hypothesis.State = investigation.HypothesisState(state)
	return hypothesis, nil
}

func scanRequest(row scanned) (investigation.Request, error) {
	var (
		request       investigation.Request
		justification *uuid.UUID
		state         int16
		refusal       *int16
		job           *uuid.UUID
		settledAt     *time.Time
	)
	if err := row.Scan(&request.ID, new(uuid.UUID), &request.RoundID, &request.Ordinal,
		&request.Pass, &request.CapabilityID, &request.CapabilityVersion, &request.Arguments,
		&justification, &request.Reason, &state, &refusal, &job, &request.ResultBytes,
		&request.ProposedAt, &settledAt); err != nil {
		return investigation.Request{}, err
	}
	if justification != nil {
		request.Justification = *justification
	}
	if refusal != nil {
		request.Refusal = investigation.Refusal(*refusal)
	}
	if job != nil {
		request.JobID = *job
	}
	if settledAt != nil {
		request.SettledAt = *settledAt
	}
	request.State = investigation.RequestState(state)
	return request, nil
}

func scanEvidence(row scanned) (investigation.Item, error) {
	var (
		item        investigation.Item
		request     *uuid.UUID
		trust       int16
		certificate []byte
		observedAt  *time.Time
	)
	if err := row.Scan(&item.ID, new(uuid.UUID), &item.RoundID, &item.Ordinal, &request,
		&item.CapabilityID, &item.CapabilityVersion, &item.Connection, &item.Statement,
		&item.Content, &item.Absence, &trust, &certificate, &observedAt,
		&item.ReceivedAt); err != nil {
		return investigation.Item{}, err
	}
	if request != nil {
		item.RequestID = *request
	}
	if observedAt != nil {
		item.SourceObservedAt = *observedAt
	}
	if len(certificate) > 0 {
		item.Certificate = &investigation.Certificate{}
		if err := json.Unmarshal(certificate, item.Certificate); err != nil {
			return investigation.Item{}, fmt.Errorf("decoding a completeness certificate: %w", err)
		}
	}
	item.Trust = investigation.TrustClass(trust)
	return item, nil
}

func scanGap(row scanned) (investigation.Gap, error) {
	var (
		gap   investigation.Gap
		cause int16
	)
	if err := row.Scan(&gap.ID, new(uuid.UUID), &gap.RoundID, &gap.Ordinal, &cause,
		&gap.CapabilityID, &gap.Subject, &gap.Consequence, &gap.RecordedAt); err != nil {
		return investigation.Gap{}, err
	}
	gap.Cause = investigation.GapCause(cause)
	return gap, nil
}

// nullableStance renders "no stance filter" as SQL NULL, which is what an unfiltered listing means.
func nullableStance(stance investigation.Stance) *int16 {
	if stance == 0 {
		return nil
	}
	value := int16(stance)
	return &value
}
