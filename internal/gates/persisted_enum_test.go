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

	"github.com/open-cluster/oc-control-plane/internal/storage"
)

// Three enums in internal/storage are persisted as integers in columns, and the same integers
// are written as bare literals inside the SQL that reads and writes those columns. Nothing in
// the language connects the two: reordering a constant block shifts every value after it while
// every literal keeps its old meaning, and the rows already stored keep the old numbering. The
// compiler cannot see it, the linter cannot see it, and it is invisible in review.
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
	}

	for _, constant := range frozen {
		if constant.got != constant.fixed {
			t.Errorf("storage.%s is %d and is stored as %d; the value is persisted in a column "+
				"and written as a literal in SQL, so changing it rewrites what existing rows mean",
				constant.name, constant.got, constant.fixed)
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
	"job.go":          {"role": connectionRoleValues},
	"lease.go":        {"status": jobStatusValues},
	"result.go":       {"status": jobStatusValues},
	"cancellation.go": {"status": jobStatusValues},
	"signal.go":       {"status": signalStatusValues},
	"connection.go":   {"role": connectionRoleValues},
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
