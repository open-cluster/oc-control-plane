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

	"github.com/open-cluster/oc-control-plane/internal/alertevent"
	"github.com/open-cluster/oc-control-plane/internal/changes"
	"github.com/open-cluster/oc-control-plane/internal/conversation"
	"github.com/open-cluster/oc-control-plane/internal/incident"
	"github.com/open-cluster/oc-control-plane/internal/integrations"
	"github.com/open-cluster/oc-control-plane/internal/investigation"
	"github.com/open-cluster/oc-control-plane/internal/store/postgres"
)

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
		{"SlackMessageReady", int(storage.SlackMessageReady), 1},
		{"SlackMessageLeased", int(storage.SlackMessageLeased), 2},
		{"SlackMessageRetry", int(storage.SlackMessageRetry), 3},
		{"SlackMessageTerminal", int(storage.SlackMessageTerminal), 4},
		{"SlackMessageComplete", int(storage.SlackMessageComplete), 5},
		{"MaxSlackMessageAttempts", storage.MaxSlackMessageAttempts, 12},

		{"AlertEventFiring", int(alertevent.AlertEventFiring), 1},
		{"AlertEventResolved", int(alertevent.AlertEventResolved), 2},

		{"StatusOpen", int(incident.StatusOpen), 1},
		{"StatusResolved", int(incident.StatusResolved), 2},
		{"BasisSourceGrouping", int(incident.BasisSourceGrouping), 1},
		{"BasisUngrouped", int(incident.BasisUngrouped), 2},

		{"InvestigationRunning", int(investigation.StatusRunning), 1},
		{"InvestigationConcluded", int(investigation.StatusConcluded), 2},
		{"InvestigationFailed", int(investigation.StatusFailed), 3},
		{"InvestigationCancelled", int(investigation.StatusCancelled), 4},
		{"RunSucceeded", int(investigation.RunSucceeded), 1},
		{"RunFailed", int(investigation.RunFailed), 2},

		{"EventStarted", int(investigation.EventStarted), 1},
		{"EventProgress", int(investigation.EventProgress), 2},
		{"EventToolStarted", int(investigation.EventToolStarted), 3},
		{"EventToolCompleted", int(investigation.EventToolCompleted), 4},
		{"EventConcluded", int(investigation.EventConcluded), 6},
		{"EventFailed", int(investigation.EventFailed), 7},
		{"EventCancelled", int(investigation.EventCancelled), 9},
		{"EventHypothesesUpdated", int(investigation.EventHypothesesUpdated), 10},

		{"KindDeployment", int(changes.KindDeployment), 1},
		{"KindStatefulSet", int(changes.KindStatefulSet), 2},
		{"KindDaemonSet", int(changes.KindDaemonSet), 3},
		{"KindConfigMap", int(changes.KindConfigMap), 4},
		{"KindSecret", int(changes.KindSecret), 5},

		{"SurfaceWeb", int(conversation.SurfaceWeb), 1},
		{"SurfaceSlack", int(conversation.SurfaceSlack), 2},
		{"StateOpen", int(conversation.StateOpen), 1},
		{"StateClosed", int(conversation.StateClosed), 2},
		{"RolePerson", int(conversation.RolePerson), 1},
		{"RoleAgent", int(conversation.RoleAgent), 2},
		{"ActorPrincipal", int(conversation.ActorPrincipal), 1},
		{"ActorExternal", int(conversation.ActorExternal), 2},

		{"ChangeBaseline", int(changes.ChangeBaseline), 1},
		{"ChangeCreated", int(changes.ChangeCreated), 2},
		{"ChangeModified", int(changes.ChangeModified), 3},
		{"ChangeDeleted", int(changes.ChangeDeleted), 4},
	}

	for _, constant := range frozen {
		if constant.got != constant.fixed {
			t.Errorf("%s is %d and is stored as %d; the value is persisted in a column and, for "+
				"some of these, written as a literal in SQL, so changing it rewrites what every "+
				"existing row means", constant.name, constant.got, constant.fixed)
		}
	}
}

func TestPersistedIntegrationStatusValuesAreFrozen(t *testing.T) {
	t.Parallel()

	if integrations.StatusVerified != "verified" {
		t.Errorf("StatusVerified = %q, want verified", integrations.StatusVerified)
	}
	if integrations.StatusFailed != "failed" {
		t.Errorf("StatusFailed = %q, want failed", integrations.StatusFailed)
	}
}

func TestRetiredInvestigationEventNumbersAreNeverReused(t *testing.T) {
	t.Parallel()

	active := []int{
		int(investigation.EventStarted), int(investigation.EventProgress),
		int(investigation.EventToolStarted), int(investigation.EventToolCompleted),
		int(investigation.EventConcluded), int(investigation.EventFailed),
		int(investigation.EventCancelled), int(investigation.EventHypothesesUpdated),
	}
	for _, retired := range []int{5, 8} {
		if containsValue(active, retired) {
			t.Errorf("retired Investigation event number %d was reused", retired)
		}
	}
}

var (
	jobStatusValues = []int{
		int(storage.JobPending), int(storage.JobLeased), int(storage.JobSucceeded),
		int(storage.JobFailed), int(storage.JobCancelled),
	}
	slackMessageStatusValues = []int{
		int(storage.SlackMessageReady), int(storage.SlackMessageLeased),
		int(storage.SlackMessageRetry), int(storage.SlackMessageTerminal),
		int(storage.SlackMessageComplete),
	}
	alertEventStatusValues = []int{
		int(alertevent.AlertEventFiring), int(alertevent.AlertEventResolved),
	}
	incidentStatusValues = []int{int(incident.StatusOpen), int(incident.StatusResolved)}
	slackReplyValues     = []int{
		int(storage.SlackReplyPending), int(storage.SlackReplyDelivering),
		int(storage.SlackReplyDelivered), int(storage.SlackReplyFailed),
	}
	investigationStatusValues = []int{
		int(investigation.StatusRunning), int(investigation.StatusConcluded),
		int(investigation.StatusFailed), int(investigation.StatusCancelled),
	}
	changeKindValues = []int{
		int(changes.ChangeBaseline), int(changes.ChangeCreated),
		int(changes.ChangeModified), int(changes.ChangeDeleted),
	}
	conversationRoleValues = []int{
		int(conversation.RolePerson), int(conversation.RoleAgent),
	}
)

var enumColumns = map[string]map[string][]int{
	"lease.go":  {"status": jobStatusValues},
	"result.go": {"status": jobStatusValues},
	"job.go": {"status": append(append([]int(nil), jobStatusValues...),
		investigationStatusValues...)},
	"relays.go":             {"status": jobStatusValues},
	"slack_message_work.go": {"status": slackMessageStatusValues},
	"webhook_delivery.go": {
		"status": slackMessageStatusValues,
	},
	"alert_event.go": {"status": alertEventStatusValues},
	"incident.go": {"status": append(append([]int(nil), incidentStatusValues...),
		alertEventStatusValues...)},
	"slack_conversation.go": {
		"status": slackMessageStatusValues,
	},
	"slack_reply.go": {"status": slackReplyValues},
	"investigation.go": {"status": append(append(append([]int(nil), investigationStatusValues...),
		incidentStatusValues...), jobStatusValues...)},
	"conversation_context.go": {
		"status": investigationStatusValues, "role": conversationRoleValues,
	},
	"investigation_lease.go": {"status": investigationStatusValues},
	"investigation_event.go": {"status": investigationStatusValues},
	"conversation.go": {
		"status": append(append([]int(nil), investigationStatusValues...),
			incidentStatusValues...),
		"role": conversationRoleValues,
	},
	"changes.go": {"change_kind": changeKindValues},
}

var scannedColumns = []string{
	"status", "outcome", "change_kind", "role",
}

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

	if inspected == 0 {
		t.Fatal("no enum literals were inspected; the gate would pass vacuously")
	}
}

func TestEnumLiteralScannerCatchesAnUndeclaredValue(t *testing.T) {
	t.Parallel()

	const sql = `UPDATE relay_job SET status = 1 WHERE status = 9`

	found := enumLiteralsFor(sql, "status")

	if !containsValue(found, 9) {
		t.Errorf("scanner read %v from %q; it must see the 9", found, sql)
	}
}

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
		{"bare in list", `AND outcome IN (1, 3)`, "outcome", []int{1, 3}},
		{"bound parameter is not a literal", `SET status = $4`, "status", nil},
		{"bound parameters in a list are not literals", `AND status IN ($8, $9)`, "status", nil},
		{"assignment from another column", `SET status = EXCLUDED.status`, "status", nil},
		{"another column entirely", `WHERE alert_event.status = 1`, "outcome", nil},
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

func enumLiteralsFor(sql, column string) []int {
	qualified := `(?i)(?:\w+\.)?\b` + regexp.QuoteMeta(column) + `\b`

	var found []int
	comparison := regexp.MustCompile(qualified + `\s*(?:=|<>|!=)\s*(\d+)\b`)
	for _, match := range comparison.FindAllStringSubmatch(sql, -1) {
		if value, err := strconv.Atoi(match[1]); err == nil {
			found = append(found, value)
		}
	}

	inList := regexp.MustCompile(qualified + `\s+IN\s*\(([^)]*)\)`)
	for _, match := range inList.FindAllStringSubmatch(sql, -1) {
		for _, entry := range strings.Split(match[1], ",") {
			if value, err := strconv.Atoi(strings.TrimSpace(entry)); err == nil {
				found = append(found, value)
			}
		}
	}
	return found
}

func sqlLiterals(file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
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

func storageProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()

	directory := filepath.Join(moduleRoot, "internal", "store", "postgres")
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
