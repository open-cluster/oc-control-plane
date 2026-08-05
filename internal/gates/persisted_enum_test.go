package gates_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Enums across this module are persisted as integers in columns, and some of the same integers are
// written as bare literals inside the SQL that reads and writes those columns. Nothing in the
// language connects the two: reordering a constant block shifts every value after it while every
// literal keeps its old meaning, and the rows already stored keep the old numbering. The compiler
// cannot see it, the linter cannot see it, and it is invisible in review.
//
// The investigation vocabulary lives in internal/investigation rather than in internal/storage,
// because the capability owns its types and persistence reconstructs them (ADR-017). Where it lives
// does not change what it is: a value in a column, constrained by a CHECK in migration 0009.
//
// Two gates cover it. The first freezes the values. The second checks that no SQL literal names
// a value no constant holds.

// The values as they are stored. A constant that moves fails here, naming itself, rather than
// silently changing what every existing row means.
//
// These are a storage contract, not an implementation detail. Changing one requires a migration
// that rewrites the column, and this table is where that decision becomes visible.
func TestPersistedEnumValuesAreFrozen(t *testing.T) {
	t.Parallel()

	frozen := []struct {
		name  string
		got   int
		fixed int
	}{
		{"JobPending", int(storage.JobPending), 0},
		{"JobLeased", int(storage.JobLeased), 1},
		{"JobSucceeded", int(storage.JobSucceeded), 2},
		{"JobFailed", int(storage.JobFailed), 3},
		{"JobCancelled", int(storage.JobCancelled), 4},

		{"SignalFiring", int(storage.SignalFiring), 1},
		{"SignalResolved", int(storage.SignalResolved), 2},

		{"RoleTrigger", int(storage.RoleTrigger), 1},
		{"RoleEvidence", int(storage.RoleEvidence), 2},
		{"RoleBoth", int(storage.RoleBoth), 3},

		// The Connection lifecycle, migration 0013's CHECKs. A value that moved here would
		// silently re-label every existing row: a Connection stored as failed would start
		// reading as degraded, which is the difference between paging somebody and not.
		{"ConnectionConfigured", int(storage.ConnectionConfigured), 1},
		{"ConnectionValidating", int(storage.ConnectionValidating), 2},
		{"ConnectionActive", int(storage.ConnectionActive), 3},
		{"ConnectionDegraded", int(storage.ConnectionDegraded), 4},
		{"ConnectionFailed", int(storage.ConnectionFailed), 5},

		// Validation results. Readiness is the COVERAGE vocabulary and is stored in
		// connection_validation_capability.readiness as well as in the authentication column.
		{"ReadyAvailable", int(storage.ReadyAvailable), 1},
		{"ReadyUnauthorized", int(storage.ReadyUnauthorized), 2},
		{"ReadyUnavailable", int(storage.ReadyUnavailable), 3},
		{"ReadyNotAttempted", int(storage.ReadyNotAttempted), 4},

		{"ValidationPassed", int(storage.ValidationPassed), 1},
		{"ValidationPartial", int(storage.ValidationPartial), 2},
		{"ValidationFailed", int(storage.ValidationFailed), 3},

		// Delivery dispositions, and the 1 that trigger_delivery's health query filters on.
		{"DeliveryAccepted", int(storage.DeliveryAccepted), 1},
		{"DeliveryDuplicate", int(storage.DeliveryDuplicate), 2},
		{"DeliveryRejected", int(storage.DeliveryRejected), 3},

		// The investigation vocabulary. These live in internal/investigation rather than in
		// internal/storage — the capability owns its types and persistence reconstructs them
		// (ADR-017) — but they are stored in columns and constrained by CHECKs in migration 0009,
		// so they are a storage contract exactly as the values above are.
		{"LifecyclePending", int(investigation.LifecyclePending), 1},
		{"LifecycleBriefing", int(investigation.LifecycleBriefing), 2},
		{"LifecycleReasoning", int(investigation.LifecycleReasoning), 3},
		{"LifecycleGathering", int(investigation.LifecycleGathering), 4},
		{"LifecycleConcluded", int(investigation.LifecycleConcluded), 5},
		{"LifecycleAbstained", int(investigation.LifecycleAbstained), 6},
		{"LifecycleCancelled", int(investigation.LifecycleCancelled), 7},
		{"LifecycleFailed", int(investigation.LifecycleFailed), 8},

		{"TriggerManual", int(investigation.TriggerManual), 1},
		{"TriggerSignal", int(investigation.TriggerSignal), 2},

		// These match the protocol's WorkloadKind. A value that moved here would ask a relay for a
		// different kind of object than the case says it is scoped to.
		{"WorkloadDeployment", int(investigation.WorkloadDeployment), 1},
		{"WorkloadStatefulSet", int(investigation.WorkloadStatefulSet), 2},
		{"WorkloadDaemonSet", int(investigation.WorkloadDaemonSet), 3},

		{"RoundConcluded", int(investigation.RoundConcluded), 1},
		{"RoundAbstained", int(investigation.RoundAbstained), 2},
		{"RoundCancelled", int(investigation.RoundCancelled), 3},
		{"RoundFailed", int(investigation.RoundFailed), 4},

		{"LimitRequests", int(investigation.LimitRequests), 1},
		{"LimitResultBytes", int(investigation.LimitResultBytes), 2},
		{"LimitDeadline", int(investigation.LimitDeadline), 3},
		{"LimitCost", int(investigation.LimitCost), 4},
		{"LimitAdaptivePasses", int(investigation.LimitAdaptivePasses), 5},

		{"HypothesisLive", int(investigation.HypothesisLive), 1},
		{"HypothesisSupported", int(investigation.HypothesisSupported), 2},
		{"HypothesisFalsified", int(investigation.HypothesisFalsified), 3},
		{"HypothesisSetAside", int(investigation.HypothesisSetAside), 4},

		{"StanceSupports", int(investigation.StanceSupports), 1},
		{"StanceContradicts", int(investigation.StanceContradicts), 2},
		{"StanceNeutral", int(investigation.StanceNeutral), 3},

		{"RequestProposed", int(investigation.RequestProposed), 1},
		{"RequestRefused", int(investigation.RequestRefused), 2},
		{"RequestDispatched", int(investigation.RequestDispatched), 3},
		{"RequestAnswered", int(investigation.RequestAnswered), 4},
		{"RequestUnproductive", int(investigation.RequestUnproductive), 5},
		{"RequestFailed", int(investigation.RequestFailed), 6},

		{"RefusedUnknownCapability", int(investigation.RefusedUnknownCapability), 1},
		{"RefusedNotPermitted", int(investigation.RefusedNotPermitted), 2},
		{"RefusedOutOfScope", int(investigation.RefusedOutOfScope), 3},
		{"RefusedOutOfWindow", int(investigation.RefusedOutOfWindow), 4},
		{"RefusedArguments", int(investigation.RefusedArguments), 5},
		{"RefusedLimitReached", int(investigation.RefusedLimitReached), 6},
		{"RefusedUnjustified", int(investigation.RefusedUnjustified), 7},
		{"RefusedConnection", int(investigation.RefusedConnection), 8},

		{"TrustCentrallyVerified", int(investigation.TrustCentrallyVerified), 1},
		{"TrustRelayAttested", int(investigation.TrustRelayAttested), 2},

		{"GapCapabilityUnavailable", int(investigation.GapCapabilityUnavailable), 1},
		{"GapCapabilityNotPermitted", int(investigation.GapCapabilityNotPermitted), 2},
		{"GapSourceUnreachable", int(investigation.GapSourceUnreachable), 3},
		{"GapAuthorizationDenied", int(investigation.GapAuthorizationDenied), 4},
		{"GapLimitReached", int(investigation.GapLimitReached), 5},
		{"GapRedactionMasked", int(investigation.GapRedactionMasked), 6},
		{"GapRetentionHorizon", int(investigation.GapRetentionHorizon), 7},
		{"GapResultTruncated", int(investigation.GapResultTruncated), 8},
		{"GapRequestRefused", int(investigation.GapRequestRefused), 9},
		{"GapTargetNotFound", int(investigation.GapTargetNotFound), 10},
		{"GapExplanationUntested", int(investigation.GapExplanationUntested), 11},

		{"CoverageChecked", int(investigation.CoverageChecked), 1},
		{"CoverageCheckedEmpty", int(investigation.CoverageCheckedEmpty), 2},
		{"CoverageIncomplete", int(investigation.CoverageIncomplete), 3},
		{"CoverageUnavailable", int(investigation.CoverageUnavailable), 4},
		{"CoverageNotApplicable", int(investigation.CoverageNotApplicable), 5},

		{"OutcomeSupported", int(investigation.OutcomeSupported), 1},
		{"OutcomeCaveated", int(investigation.OutcomeCaveated), 2},
		{"OutcomeAbstained", int(investigation.OutcomeAbstained), 3},

		{"ClaimSupporting", int(investigation.ClaimSupporting), 1},
		{"ClaimContradicting", int(investigation.ClaimContradicting), 2},
		{"ClaimAffectedScope", int(investigation.ClaimAffectedScope), 3},
	}

	for _, constant := range frozen {
		if constant.got != constant.fixed {
			t.Errorf("%s is %d and is stored as %d; the value is persisted in a column and, for "+
				"some of these, written as a literal in SQL, so changing it rewrites what every "+
				"existing row means", constant.name, constant.got, constant.fixed)
		}
	}
}

// The value sets, named once so a file below says which enum governs it rather than restating
// the numbers.
var (
	jobStatusValues = []int{
		int(storage.JobPending), int(storage.JobLeased), int(storage.JobSucceeded),
		int(storage.JobFailed), int(storage.JobCancelled),
	}
	signalStatusValues   = []int{int(storage.SignalFiring), int(storage.SignalResolved)}
	connectionRoleValues = []int{
		int(storage.RoleTrigger), int(storage.RoleEvidence), int(storage.RoleBoth),
	}
	connectionStateValues = []int{
		int(storage.ConnectionConfigured), int(storage.ConnectionValidating),
		int(storage.ConnectionActive), int(storage.ConnectionDegraded),
		int(storage.ConnectionFailed),
	}
	deliveryOutcomeValues = []int{
		int(storage.DeliveryAccepted), int(storage.DeliveryDuplicate),
		int(storage.DeliveryRejected),
	}
)

// enumColumns maps a file in internal/storage to the values its SQL may compare each enum
// column against. Two different enums are both stored in a column called status — a job's and a
// signal's — so the legal set is decided per file rather than per column name. A gate that
// pooled them would assert a property that is not true.
//
// A file is listed here when its SQL names one of these columns. Adding SQL that does so to any
// other file fails the gate below, which is the point: the new file has to say which enum
// governs it. That is not hypothetical — splitting the job queries into these files is what
// first fired it.
var enumColumns = map[string]map[string][]int{
	"job.go": {"role": connectionRoleValues},
	// The investigator reaches a customer's cluster through a Connection, so the same role check
	// guards opening a case and dispatching one of its reads. Cancelling a case also touches
	// relay_job's status, which is why that file names two enums.
	"investigation.go":      {"role": connectionRoleValues, "status": jobStatusValues},
	"investigation_pack.go": {"role": connectionRoleValues},
	"lease.go":              {"status": jobStatusValues},
	"result.go":             {"status": jobStatusValues},
	"cancellation.go":       {"status": jobStatusValues},
	"signal.go":             {"status": signalStatusValues},
	"connection.go":         {"role": connectionRoleValues},
	// The lifecycle files. connection_lifecycle.go compares `role` when it counts what depends
	// on a Connection and writes `state` when a validation lands; trigger_delivery.go compares
	// `outcome` in every health query. Each names the enum that governs it rather than relying
	// on the column name, because `outcome` is also a validation's own column.
	"connection_lifecycle.go": {"role": connectionRoleValues, "state": connectionStateValues},
	"trigger_delivery.go":     {"outcome": deliveryOutcomeValues, "role": connectionRoleValues},
	// The fleet counts leased jobs to report what the relays are holding.
	"fleet.go": {"status": jobStatusValues},
}

// Every integer an enum column is compared against must be a value some constant holds. This
// catches the literal gate one cannot see: a typed 5 where 4 was meant, or a value invented for
// a state that was never declared.
//
// What it does not see, stated rather than implied: a value that appears somewhere other than a
// comparison against the column — the 4 in "CASE WHEN status = 0 THEN 4" is assigned, not
// compared, and is not read here. Gate one is what covers that one, by making 4 mean what it has
// always meant.
func TestSQLComparesEnumColumnsOnlyToDeclaredValues(t *testing.T) {
	t.Parallel()

	inspected := 0
	for name, file := range storageProductionFiles(t) {
		for _, sql := range sqlLiterals(file) {
			for _, column := range []string{"status", "role"} {
				literals := enumLiteralsFor(sql, column)
				if len(literals) == 0 {
					continue
				}
				legal, declared := enumColumns[name][column]
				if !declared {
					t.Errorf("%s compares %s to %v, but no enum is recorded for that column in "+
						"this file; add it to enumColumns so the values are checked",
						name, column, literals)
					continue
				}
				inspected += len(literals)
				for _, literal := range literals {
					if !containsValue(legal, literal) {
						// The wording matters when the value is new rather than wrong. A
						// constant that was genuinely added is declared in Go and still fails
						// here until it is recorded, and the message has to say so or it reads
						// as the gate being broken.
						t.Errorf("%s compares %s to %d; the values recorded for that column are "+
							"%v. If a constant was added, record it above — these values are a "+
							"storage contract, and extending them is a decision",
							name, column, literal, legal)
					}
				}
			}
		}
	}

	// A gate that read nothing has stopped working rather than found nothing wrong.
	if inspected == 0 {
		t.Fatal("no enum literals were inspected; the gate would pass vacuously")
	}
}

// The scanner must report a literal that no declared constant holds. This is the violation the
// gate exists for, and testing it against a fixture is what stops the gate passing because its
// detection never worked rather than because the tree is clean.
func TestEnumLiteralScannerCatchesAnUndeclaredValue(t *testing.T) {
	t.Parallel()

	const sql = `UPDATE relay_job SET status = 1 WHERE status = 9`

	found := enumLiteralsFor(sql, "status")

	if !containsValue(found, 9) {
		t.Errorf("scanner read %v from %q; it must see the 9", found, sql)
	}
}

// The forms the scanner has to read, and the ones it must leave alone. A scanner that reported a
// bound parameter as a literal would fail the gate on correct SQL, which is the failure that
// gets a gate deleted.
func TestEnumLiteralScannerReadsTheFormsInUse(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		sql    string
		column string
		want   []int
	}{
		{"qualified equality", `AND held.status = 1`, "status", []int{1}},
		{"disjunction", `AND (status = 0 OR (status = 1 AND at <= now()))`, "status", []int{0, 1}},
		{"in list", `AND connection.role          IN (2, 3)`, "role", []int{2, 3}},
		{"bare in list", `AND role IN (1, 3)`, "role", []int{1, 3}},
		{"bound parameter is not a literal", `SET status = $4`, "status", nil},
		{"assignment from another column", `SET status = EXCLUDED.status`, "status", nil},
		{"another column entirely", `WHERE signal.status = 1`, "role", nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got := enumLiteralsFor(testCase.sql, testCase.column)
			if len(got) != len(testCase.want) {
				t.Fatalf("read %v from %q; want %v", got, testCase.sql, testCase.want)
			}
			for _, wanted := range testCase.want {
				if !containsValue(got, wanted) {
					t.Errorf("read %v from %q; it must see the %d", got, testCase.sql, wanted)
				}
			}
		})
	}
}

// enumLiteralsFor reports the integer literals one SQL fragment compares a column against,
// reading the equality, inequality and IN forms this codebase uses. A table qualifier is
// tolerated because the SQL uses aliases; a bound parameter is not a literal and is skipped.
func enumLiteralsFor(sql, column string) []int {
	qualified := `(?i)(?:\w+\.)?\b` + regexp.QuoteMeta(column) + `\b`

	var found []int
	comparison := regexp.MustCompile(qualified + `\s*(?:=|<>|!=)\s*(\d+)\b`)
	for _, match := range comparison.FindAllStringSubmatch(sql, -1) {
		if value, err := strconv.Atoi(match[1]); err == nil {
			found = append(found, value)
		}
	}

	digits := regexp.MustCompile(`\d+`)
	inList := regexp.MustCompile(qualified + `\s+IN\s*\(([^)]*)\)`)
	for _, match := range inList.FindAllStringSubmatch(sql, -1) {
		for _, literal := range digits.FindAllString(match[1], -1) {
			if value, err := strconv.Atoi(literal); err == nil {
				found = append(found, value)
			}
		}
	}
	return found
}

// sqlLiterals returns the string literals in one file that look like SQL. Every query in
// internal/storage is a raw literal passed to pgx, so this is where the column comparisons are.
func sqlLiterals(file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			// A raw string literal containing a backquote cannot occur, so the only unquote
			// failures here are literals this walk has no interest in.
			return true
		}
		if looksLikeSQL(value) {
			found = append(found, value)
		}
		return true
	})
	return found
}

func looksLikeSQL(value string) bool {
	upper := strings.ToUpper(value)
	for _, keyword := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE "} {
		if strings.Contains(upper, keyword) {
			return true
		}
	}
	return false
}

// storageProductionFiles parses internal/storage by file name, which the gate needs because the
// legal value set is decided per file. The shared helper discards names.
func storageProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()

	directory := filepath.Join("..", "storage")
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("reading %s: %v", directory, err)
	}

	fileSet := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatalf("%s contains no production Go files; the gate would pass vacuously", directory)
	}
	return files
}

func containsValue(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
