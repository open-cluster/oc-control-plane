package investigation

import (
	"strings"
	"testing"
)

func TestSubjectTermsDropNoiseAndDuplicates(t *testing.T) {
	terms := subjectTerms("What is wrong with the payments PAYMENTS pod?", "payments")
	joined := strings.Join(terms, " ")
	if strings.Count(joined, "payments") != 1 {
		t.Errorf("terms = %v; duplicates must collapse", terms)
	}
	for _, noise := range []string{"what", "the", "is"} {
		if strings.Contains(" "+joined+" ", " "+noise+" ") {
			t.Errorf("terms = %v carry the noise word %q", terms, noise)
		}
	}
	if !strings.Contains(joined, "pod") {
		t.Errorf("terms = %v; a three-letter identifier is signal", terms)
	}
}
