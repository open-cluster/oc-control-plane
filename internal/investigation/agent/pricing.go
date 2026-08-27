package reasoning

import (
	"fmt"
	"sort"
	"strings"
)

// What a round cost, from four rates rather than two.
//
// Input, output, cache write and cache read are priced differently — a cache read is about a tenth
// of input and a cache write more than it. Costing a round from input and output alone reports the
// cheapest rounds as the most expensive, which makes the cost ceiling fire on the wrong rounds and
// gets the price of the feature wrong in the direction that loses money.

// microCentsPerMicroDollar converts a published price into the unit this system counts in. Money
// is integer micro-cents everywhere, because a float would make two runs that cost the same report
// different totals.
const microCentsPerMicroDollar = 100

// perMillion turns a vendor's published price — US dollars per million tokens, given in millionths
// of a dollar so the declaration stays an integer — into micro-cents per million tokens.
//
// Prices are declared this way rather than as decimals because a rate written as a float would be
// the one place money entered this system as an approximation, and every figure downstream of it
// inherits that.
func perMillion(microDollars int64) int64 {
	return microDollars * microCentsPerMicroDollar
}

// Rate is one model's four prices, in micro-cents per million tokens.
type Rate struct {
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
}

// Cost prices one call. Unreported figures contribute nothing, which is correct: a provider that
// does not report cache writes did not perform a cache write this system knows how to charge for.
//
// Rounding is half-up rather than truncating, so a long run of small calls does not accumulate a
// systematic under-report of what was actually spent.
func (r Rate) Cost(usage TokenUsage) int64 {
	total := usage.Input.Or(0)*r.Input +
		usage.Output.Or(0)*r.Output +
		usage.CacheWrite.Or(0)*r.CacheWrite +
		usage.CacheRead.Or(0)*r.CacheRead
	return (total + tokensPerMillion/2) / tokensPerMillion
}

const tokensPerMillion = 1_000_000

// Tariff is the declared rate table, keyed by provider and exact model identifier.
//
// A model with no entry is a REFUSAL rather than a cost of zero. Zero silently disables the cost
// ceiling, and the mistake would first be noticed as a bill — so the failure is moved to startup,
// where the person who chose the model is still the person reading the error.
type Tariff struct {
	rates map[string]Rate
}

// NewTariff builds a rate table from declared entries.
func NewTariff(rates map[string]Rate) Tariff {
	declared := make(map[string]Rate, len(rates))
	for key, rate := range rates {
		declared[key] = rate
	}
	return Tariff{rates: declared}
}

// Lookup returns the rate for one deployment, refusing a model nobody declared a price for.
func (t Tariff) Lookup(provider, model string) (Rate, error) {
	rate, priced := t.rates[tariffKey(provider, model)]
	if !priced {
		return Rate{}, fmt.Errorf(
			"%w: %s/%s is not in the rate table, and a round costed at zero would silently "+
				"disable the cost ceiling; declare its four rates or configure a model that is "+
				"already declared (known: %s)",
			ErrUnpriced, provider, model, strings.Join(t.Declared(), ", "))
	}
	return rate, nil
}

// Declared lists every priced deployment, so a refusal can say what the alternatives are rather
// than only what was wrong.
func (t Tariff) Declared() []string {
	known := make([]string, 0, len(t.rates))
	for key := range t.rates {
		known = append(known, key)
	}
	sort.Strings(known)
	return known
}

func tariffKey(provider, model string) string {
	return provider + "/" + model
}

// DefaultTariff is the rate table this build ships, from each vendor's published prices.
//
// It is a committed artifact rather than a remote lookup on purpose: a price that changed under a
// running deployment would silently change what every recorded round is comparable against, and
// the diff that updates a rate is the record of when the comparison stopped being like-for-like.
func DefaultTariff() Tariff {
	return NewTariff(map[string]Rate{
		// Anthropic, published in US dollars per million tokens. Cache reads are a tenth of
		// input and cache writes a quarter more than it.
		"anthropic/claude-opus-5": {
			Input:      perMillion(5_000_000),  // $5.00
			Output:     perMillion(25_000_000), // $25.00
			CacheWrite: perMillion(6_250_000),  // $6.25, input plus a quarter
			CacheRead:  perMillion(500_000),    // $0.50, a tenth of input
		},
		"anthropic/claude-opus-4-8": {
			Input:      perMillion(5_000_000),
			Output:     perMillion(25_000_000),
			CacheWrite: perMillion(6_250_000),
			CacheRead:  perMillion(500_000),
		},
		"anthropic/claude-sonnet-5": {
			Input:      perMillion(3_000_000),  // $3.00
			Output:     perMillion(15_000_000), // $15.00
			CacheWrite: perMillion(3_750_000),
			CacheRead:  perMillion(300_000),
		},
		"anthropic/claude-haiku-4-5": {
			Input:      perMillion(1_000_000), // $1.00
			Output:     perMillion(5_000_000), // $5.00
			CacheWrite: perMillion(1_250_000),
			CacheRead:  perMillion(100_000),
		},

		// Z.AI, published in US dollars per million tokens. The cache-write column is the input
		// rate because this vendor publishes no separate write price and reports no write count:
		// nothing is ever charged against it, and pricing it at zero would be a claim rather
		// than an absence.
		"zai/glm-4.6": {
			Input:      perMillion(600_000),   // $0.60
			Output:     perMillion(2_200_000), // $2.20
			CacheWrite: perMillion(600_000),
			CacheRead:  perMillion(110_000), // $0.11
		},
		"zai/glm-4.7": {
			Input:      perMillion(600_000),
			Output:     perMillion(2_200_000),
			CacheWrite: perMillion(600_000),
			CacheRead:  perMillion(110_000),
		},
		"zai/glm-5": {
			Input:      perMillion(1_000_000), // $1.00
			Output:     perMillion(3_200_000), // $3.20
			CacheWrite: perMillion(1_000_000),
			CacheRead:  perMillion(200_000), // $0.20
		},
		"zai/glm-5.2": {
			Input:      perMillion(1_400_000), // $1.40
			Output:     perMillion(4_400_000), // $4.40
			CacheWrite: perMillion(1_400_000),
			CacheRead:  perMillion(260_000), // $0.26
		},
	})
}
