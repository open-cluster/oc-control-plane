package identity

import "testing"

func TestPasswordVerifierIsVersionedAndRequestsParameterUpgrade(t *testing.T) {
	const password = "correct horse battery staple"
	old := encodePassword(password, []byte("sixteen byte salt"), 32*1024, 2, 2)
	valid, rehash, err := verifyPassword(old, password)
	if err != nil || !valid || !rehash {
		t.Fatalf("old verifier = valid %v, rehash %v, error %v", valid, rehash, err)
	}
	valid, _, err = verifyPassword(old, "different password")
	if err != nil || valid {
		t.Fatalf("wrong password = valid %v, error %v", valid, err)
	}
}
