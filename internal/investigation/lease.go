package investigation

import "time"

const (
	heartbeatInterval = 2 * time.Minute
	leaseDuration     = 15 * time.Minute
	claimInterval     = 500 * time.Millisecond
	sweepInterval     = time.Minute
	sweepBatch        = 50
)

// RecoveryReason is terminal because model transcripts are not durable enough to resume safely.
const RecoveryReason = "worker interrupted"

type Claim struct {
	Worker   string
	LeaseFor time.Duration
}
