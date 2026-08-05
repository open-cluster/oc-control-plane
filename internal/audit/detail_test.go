package audit_test

import (
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/audit"
)

// The detail column is the one place in this table a caller supplies free-form structure, and
// it is written on paths that are holding a secret at the time — a Connection being created
// with its shared secret, a provider being configured with its client secret. A call site that
// forgot is the failure this exists to make impossible, so the dropping is mechanical rather
// than remembered.
func TestDetailDropsAnythingNamedLikeACredential(t *testing.T) {
	t.Parallel()

	detail := audit.Detail{
		"clientSecret":  "the-actual-secret",
		"secret":        "another",
		"apiToken":      "opc_live_1234",
		"password":      "hunter2",
		"credential":    "bearer xyz",
		"privateKey":    "-----BEGIN",
		"secretDigest":  "not the secret, but nobody reading this needs it",
		"authorization": "Bearer abc",
		"environmentId": "e0000000-0000-4000-8000-000000000000",
	}

	safe := detail.Safe()

	for _, dropped := range []string{
		"clientSecret", "secret", "apiToken", "password", "credential",
		"privateKey", "secretDigest", "authorization",
	} {
		if _, present := safe[dropped]; present {
			t.Errorf("%q survived; the record must never hold a credential, and this column is "+
				"written on the paths that are holding one", dropped)
		}
	}
	if safe["environmentId"] != "e0000000-0000-4000-8000-000000000000" {
		t.Errorf("an ordinary identifier was dropped: %v", safe["environmentId"])
	}
}

// An event is written on a path a caller partly controls, so the values are bounded. Without
// this an attacker-chosen string repeated without limit is a storage amplifier against a table
// nothing is allowed to delete from.
func TestDetailBoundsWhatOneEntryCanHold(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", audit.MaxDetailValueLength*3)
	safe := audit.Detail{"reason": long}.Safe()

	value, ok := safe["reason"].(string)
	if !ok {
		t.Fatalf("reason came back as %T, want a string", safe["reason"])
	}
	if len(value) > audit.MaxDetailValueLength+len("…") {
		t.Errorf("a %d-byte value was kept whole; the table nothing may delete from is not "+
			"somewhere a caller stores whatever they like", len(value))
	}
}

func TestDetailBoundsHowManyEntriesOneEventCarries(t *testing.T) {
	t.Parallel()

	crowded := audit.Detail{}
	for index := range audit.MaxDetailEntries * 2 {
		crowded[string(rune('a'+index%26))+strings.Repeat("x", index)] = index
	}

	if kept := len(crowded.Safe()); kept > audit.MaxDetailEntries {
		t.Errorf("kept %d entries, want at most %d", kept, audit.MaxDetailEntries)
	}
}

// The zero Detail is the ordinary case — most events carry no structure at all — and it must
// not become an empty object nobody asked for or a nil dereference.
func TestSafeOfNothingIsNothing(t *testing.T) {
	t.Parallel()

	if safe := audit.Detail(nil).Safe(); safe != nil {
		t.Errorf("the zero detail became %v", safe)
	}
}
