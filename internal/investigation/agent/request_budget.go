package agent

type requestEstimator interface {
	EstimateRequestTokens(Prompt) (int, error)
}

// EstimateSerializedRequest is a coarse byte-derived token estimate, not measured usage.
func EstimateSerializedRequest(encoded []byte) int {
	base := len(encoded)
	return base + (base+9)/10
}
