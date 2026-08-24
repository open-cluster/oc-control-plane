package identity

import (
	"net/http/httptest"
	"testing"
)

func TestOnlyASameSitePathSurvivesAsAReturnTarget(t *testing.T) {
	handlers := Handlers{}
	for _, testCase := range []struct {
		asked    string
		accepted bool
	}{
		{"", true}, {"/investigations/1", true}, {"/investigations?status=open", true},
		{"https://evil.example.com/", false}, {"//evil.example.com/", false},
		{"/\\evil.example.com", false}, {"evil.example.com", false},
		{"javascript:alert(1)", false},
	} {
		recorder := httptest.NewRecorder()
		_, accepted := handlers.returnTarget(recorder, testCase.asked)
		if accepted != testCase.accepted {
			t.Errorf("returnTo=%q accepted=%v, want %v", testCase.asked, accepted, testCase.accepted)
		}
	}
}

func TestAnIssuerMustBeHTTPSOrLoopback(t *testing.T) {
	for _, testCase := range []struct {
		issuer   string
		accepted bool
	}{
		{"https://login.example.com", true}, {"https://login.example.com/tenant/1", true},
		{"http://127.0.0.1:8080", true}, {"http://localhost:8080", true},
		{"http://login.example.com", false}, {"ftp://login.example.com", false},
		{"login.example.com", false}, {"", false},
	} {
		accepted := usableIssuer(testCase.issuer) == nil
		if accepted != testCase.accepted {
			t.Errorf("%q accepted=%v, want %v", testCase.issuer, accepted, testCase.accepted)
		}
	}
}
