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

	"github.com/open-cluster/oc-control-plane/internal/changeledger"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Enums across this module are persisted as integers in columns, and some of the same integers are
// written as bare literals inside the SQL that reads and writes those columns. Nothing in the
// language connects the two: reordering a constant block shifts every value after it while every
// literal keeps its old meaning, and the rows already stored keep the old numbering. The compiler
// cannot see it, the linter cannot see it, and it is invisible in review.
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

		// The Integration Type ids are seeded into integration_type by migration and
		// compiled here as constants; the id is the join key everything else stores.
		{"TypeAlertmanager", int(integrations.TypeAlertmanager), 1},
		{"TypeKubernetes", int(integrations.TypeKubernetes), 2},
		{"TypeSlack", int(integrations.TypeSlack), 3},
		{"TypeGitHub", int(integrations.TypeGitHub), 4},

		// An Integration's observed status. A value that moved here would silently
		// re-label every existing row: an Integration stored as failed would start reading
		// as degraded, which is the difference between paging somebody and not.
		{"StatusConfigured", int(integrations.StatusConfigured), 1},
		{"StatusActive", int(integrations.StatusActive), 2},
		{"StatusDegraded", int(integrations.StatusDegraded), 3},
		{"StatusFailed", int(integrations.StatusFailed), 4},

		// Delivery dispositions, and the 1 the delivery health queries filter on.
		{"DeliveryAccepted", int(storage.DeliveryAccepted), 1},
		{"DeliveryDuplicate", int(storage.DeliveryDuplicate), 2},
		{"DeliveryRejected", int(storage.DeliveryRejected), 3},

		// An episode's vocabulary is the capability's rather than persistence's, because
		// the capability owns what it defines. What is frozen is the same thing either
		// way: the integers the SQL writes as bare literals.
		{"StatusOpen", int(incident.StatusOpen), 1},
		{"StatusResolved", int(incident.StatusResolved), 2},
		{"BasisSourceGrouping", int(incident.BasisSourceGrouping), 1},
		{"BasisUngrouped", int(incident.BasisUngrouped), 2},

		// An investigation's lifecycle and its runs' outcomes. The still-running guard in
		// the ending update is written as `status = 1`.
		{"InvestigationRunning", int(investigation.StatusRunning), 1},
		{"InvestigationConcluded", int(investigation.StatusConcluded), 2},
		{"InvestigationFailed", int(investigation.StatusFailed), 3},
		{"RunSucceeded", int(investigation.RunSucceeded), 1},
		{"RunFailed", int(investigation.RunFailed), 2},

		// The investigation event stream's vocabulary. The column CHECK is written as a
		// range, so a value that moved would still be stored and would simply mean
		// something else to every reader.
		{"EventStarted", int(investigation.EventStarted), 1},
		{"EventProgress", int(investigation.EventProgress), 2},
		{"EventToolStarted", int(investigation.EventToolStarted), 3},
		{"EventToolCompleted", int(investigation.EventToolCompleted), 4},
		{"EventAnswerDelta", int(investigation.EventAnswerDelta), 5},
		{"EventConcluded", int(investigation.EventConcluded), 6},
		{"EventFailed", int(investigation.EventFailed), 7},
		{"EventCompacted", int(investigation.EventCompacted), 8},

		// The change ledger's vocabulary. The baseline exclusion in every change query is
		// written as `change_kind <> 1`, so ChangeBaseline moving would silently turn
		// every baseline into a reportable change.
		{"KindDeployment", int(changeledger.KindDeployment), 1},
		{"KindStatefulSet", int(changeledger.KindStatefulSet), 2},
		{"KindDaemonSet", int(changeledger.KindDaemonSet), 3},
		{"KindConfigMap", int(changeledger.KindConfigMap), 4},
		{"KindSecret", int(changeledger.KindSecret), 5},

		// A conversation's own vocabularies. Every one is written as a bare literal in
		// the SQL that reads or writes it, so a constant that moved would silently
		// re-label rows: a person's message would start reading as the agent's.
		{"SurfaceWeb", int(conversation.SurfaceWeb), 1},
		{"SurfaceSlack", int(conversation.SurfaceSlack), 2},
		{"StateOpen", int(conversation.StateOpen), 1},
		{"StateClosed", int(conversation.StateClosed), 2},
		{"RolePerson", int(conversation.RolePerson), 1},
		{"RoleAgent", int(conversation.RoleAgent), 2},
		{"ActorPrincipal", int(conversation.ActorPrincipal), 1},
		{"ActorExternal", int(conversation.ActorExternal), 2},

		{"ChangeBaseline", int(changeledger.ChangeBaseline), 1},
		{"ChangeCreated", int(changeledger.ChangeCreated), 2},
		{"ChangeModified", int(changeledger.ChangeModified), 3},
		{"ChangeDeleted", int(changeledger.ChangeDeleted), 4},
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
	signalStatusValues    = []int{int(storage.SignalFiring), int(storage.SignalResolved)}
	deliveryOutcomeValues = []int{
		int(storage.DeliveryAccepted), int(storage.DeliveryDuplicate),
		int(storage.DeliveryRejected),
	}
	episodeStatusValues = []int{int(incident.StatusOpen), int(incident.StatusResolved)}
	// A Slack delivery's own lifecycle, which is NOT an investigation's: it is pending,
	// delivering, delivered or failed, and a delivery that failed says nothing about the
	// investigation behind it.
	slackDeliveryValues = []int{
		int(storage.SlackDeliveryPending), int(storage.SlackDeliveryDelivering),
		int(storage.SlackDeliveryDelivered), int(storage.SlackDeliveryFailed),
	}
	investigationStatusValues = []int{
		int(investigation.StatusRunning), int(investigation.StatusConcluded),
		int(investigation.StatusFailed),
	}
	integrationTypeValues = []int{
		int(integrations.TypeAlertmanager), int(integrations.TypeKubernetes),
		int(integrations.TypeSlack), int(integrations.TypeGitHub),
	}
	changeKindValues = []int{
		int(changeledger.ChangeBaseline), int(changeledger.ChangeCreated),
		int(changeledger.ChangeModified), int(changeledger.ChangeDeleted),
	}
	conversationRoleValues = []int{
		int(conversation.RolePerson), int(conversation.RoleAgent),
	}
)

// enumColumns maps a file in internal/storage to the values its SQL may compare each enum
// column against. Two different enums are both stored in a column called status — a job's, a
// signal's and an episode's — so the legal set is decided per file rather than per column name.
//
// A file is listed here when its SQL compares one of the scanned columns to a literal. Adding
// SQL that does so to any other file fails the gate below, which is the point: the new file has
// to say which enum governs it.
var enumColumns = map[string]map[string][]int{
	"lease.go":        {"status": jobStatusValues},
	"result.go":       {"status": jobStatusValues},
	"cancellation.go": {"status": jobStatusValues},
	// The fleet counts leased jobs to report what the relays are holding.
	"fleet.go": {"status": jobStatusValues},
	// The delivery path: the upsert guard compares a SIGNAL's status, and the idempotence
	// key's partial-index predicate compares a delivery's outcome.
	"signal.go": {"status": signalStatusValues, "outcome": deliveryOutcomeValues},
	// Grouping compares an EPISODE's status — an open episode is the one a new Signal joins —
	// and recomputing one counts the signals still firing, which shares the value 1.
	"incident.go": {"status": append(append([]int(nil), episodeStatusValues...),
		signalStatusValues...)},
	// The last-accepted-delivery health read filters on the accepted outcome.
	"integration.go": {"outcome": deliveryOutcomeValues},
	// An inbound Slack message claims its delivery through the same idempotence key every
	// other delivery uses, so it writes and compares the accepted outcome.
	"slack_conversation.go": {"outcome": deliveryOutcomeValues},
	// The outbound half: claiming compares a delivery's own lifecycle state.
	"slack_delivery.go": {"status": slackDeliveryValues},
	// The ending update is guarded on the investigation still running, and the
	// open-episode listing filters on an EPISODE's status; the two enums share the file.
	"investigation.go": {"status": append(append([]int(nil), investigationStatusValues...),
		episodeStatusValues...)},
	// The brief carries only what CONCLUDED turns established: a running turn has
	// established nothing yet, and a failed one established nothing at all.
	"conversation_brief.go": {"status": investigationStatusValues},
	// Claiming, renewing and sweeping all guard on the investigation still running, and
	// the recovery sweep fails it — so the file writes an investigation status as a
	// literal twice, in the two places that mean the most.
	"investigation_lease.go": {"status": investigationStatusValues},
	// Opening a turn counts INVESTIGATIONS that are still running and derives the window
	// from whether the EPISODE is still open, so the same two enums share this file too.
	// The queued-message reads filter on a MESSAGE's role, because only what a person
	// said becomes the turn's question.
	"conversation.go": {
		"status": append(append([]int(nil), investigationStatusValues...),
			episodeStatusValues...),
		"role": conversationRoleValues,
	},
	// The ledger opens scopes only for kubernetes Integrations and excludes baselines from
	// every change query.
	"change_ledger.go": {
		"integration_type_id": integrationTypeValues,
		"change_kind":         changeKindValues,
	},
}

// scannedColumns is every column name the gate reads comparisons against. A column absent from
// this list is invisible to the gate, so extending the persisted vocabulary starts here.
var scannedColumns = []string{
	"status", "outcome", "integration_type_id", "change_kind", "role",
}

// Every integer an enum column is compared against must be a value some constant holds. This
// catches the literal gate one cannot see: a typed 5 where 4 was meant, or a value invented for
// a state that was never declared.
func TestSQLComparesEnumColumnsOnlyToDeclaredValues(t *testing.T) {
	t.Parallel()

	inspected := 0
	for name, file := range storageProductionFiles(t) {
		for _, sql := range sqlLiterals(file) {
			for _, column := range scannedColumns {
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
		{"in list", `AND integration.integration_type_id IN (2, 3)`,
			"integration_type_id", []int{2, 3}},
		{"bare in list", `AND outcome IN (1, 3)`, "outcome", []int{1, 3}},
		{"bound parameter is not a literal", `SET status = $4`, "status", nil},
		{"assignment from another column", `SET status = EXCLUDED.status`, "status", nil},
		{"another column entirely", `WHERE signal.status = 1`, "outcome", nil},
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
