package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// ONE table test over every list endpoint that speaks the shared contract.
//
// It is one test rather than one per endpoint because that is the whole claim being made: the
// frontend has one table to build and one contract to test, and a listing that quietly answered
// a different shape would be a second contract nobody wrote down. A test per endpoint would
// assert the same four properties five times and would still not say they were the same four.
//
// The four properties are the ones a client depends on and cannot check for itself:
//
//  1. An unknown sort field is REFUSED rather than ignored. Ignored, it returns rows in an order
//     nobody chose and the caller has no way to notice.
//  2. A tampered cursor answers a refusal rather than starting over. Starting over shows an
//     operator the first page again and lets them believe they have seen the last.
//  3. A limit above the ceiling is CLAMPED rather than refused, because somebody asking for more
//     than they can have wants as much as they can have.
//  4. The envelope is the same shape every time, including its empty spellings: `items` is `[]`
//     and never null, `next` and `total` are explicitly null rather than absent.
func TestEveryListEndpointSpeaksOneTableContract(t *testing.T) {
	plane := startIntegrationPlane(t)
	base := plane.base(surfaceOrg)

	// Something to list, so a listing that is empty for the wrong reason is not mistaken for a
	// contract that holds.
	plane.createAlertmanager(t, "Contract Alertmanager")

	listings := map[string]string{
		"integrations":   base + "/integrations",
		"relays":         base + "/relays",
		"incidents":      base + "/incidents",
		"investigations": base + "/investigations",
		"relay integrations": base + "/relays/" + plane.relay.registration.String() +
			"/integrations",
	}

	for name, url := range listings {
		t.Run(name+": the envelope is the same shape", func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url, nil)
			if status != http.StatusOK {
				t.Fatalf("%s = %d: %s", url, status, body)
			}
			// Decoded into a raw map, because what is under test is the SHAPE — that the keys
			// are present and that the empty ones are spelled the way the contract says.
			var envelope map[string]json.RawMessage
			decodeInto(t, body, &envelope)

			for _, key := range []string{"items", "next", "total", "partial"} {
				if _, present := envelope[key]; !present {
					t.Errorf("%s answered without %q; every field is present including the "+
						"empty ones, so a client does not have to handle two spellings of "+
						"nothing: %s", name, key, body)
				}
			}
			if string(envelope["items"]) == "null" {
				t.Errorf("%s answered items as null rather than []", name)
			}
			if string(envelope["partial"]) == "null" {
				t.Errorf("%s answered partial as null rather than []", name)
			}
		})

		t.Run(name+": an unknown sort field is refused", func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url+"?sort=-notAField", nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s?sort=-notAField = %d, want 400: a sort silently dropped returns "+
					"rows in an order nobody chose: %s", url, status, body)
			}
		})

		t.Run(name+": an unknown filter is refused", func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url+"?notAFilter=anything", nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s?notAFilter=anything = %d, want 400: a filter silently dropped "+
					"returns everything while looking narrowed: %s", url, status, body)
			}
		})

		t.Run(name+": a tampered cursor is refused", func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url+"?cursor=tampered", nil)
			if status != http.StatusBadRequest {
				t.Errorf("%s?cursor=tampered = %d, want 400: silently starting over lets an "+
					"operator believe they have seen the last page: %s", url, status, body)
			}
		})

		t.Run(name+": a limit above the ceiling is clamped, not refused", func(t *testing.T) {
			status, body := plane.call(t, http.MethodGet, url+"?limit=1000000", nil)
			if status != http.StatusOK {
				t.Errorf("%s?limit=1000000 = %d, want it clamped and served: %s",
					url, status, body)
			}
		})
	}
}
