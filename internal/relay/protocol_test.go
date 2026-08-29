package relay

import "testing"

func TestProtocolCompatibilityRequiresTheCurrentVersionOrNewer(t *testing.T) {
	for version, want := range map[uint32]bool{0: false, 1: true, 2: true} {
		if got := supportsProtocol(version); got != want {
			t.Errorf("supportsProtocol(%d) = %t, want %t", version, got, want)
		}
	}
}
