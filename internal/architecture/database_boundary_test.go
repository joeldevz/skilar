package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// directDatabaseAccessBaseline freezes the pre-refactor debt. Counts may go
// down as repositories are extracted, but they must never go up and no new
// production file may start depending on workflow.SQLiteStore.Database().
var directDatabaseAccessBaseline = map[string]int{
	"cmd/skynex/workflow.go":                    19,
	"cmd/skynex/workflow_detach.go":             1,
	"cmd/skynex/workflow_replan.go":             3,
	"cmd/skynex/workflow_retry_verification.go": 6,
	"cmd/skynex/workflow_run.go":                28,
	"internal/execution/execution.go":           7,
	"internal/execution/opencode.go":            9,
	"internal/orchestration/orchestration.go":   1,
	"internal/review/runner.go":                 21,
	"internal/verification/verification.go":     3,
}

// New direct database access is allowed only inside the future persistence
// adapter. Feature and application packages must depend on consumer-owned
// ports instead of exposing *sql.DB.
var directDatabaseAdapterRoots = []string{
	"internal/persistence/sqlite/",
}

func TestNoNewDirectWorkflowDatabaseAccess(t *testing.T) {
	repo := repositoryRoot(t)
	actual := map[string]int{}
	for _, root := range []string{"cmd/skynex", "internal"} {
		err := filepath.WalkDir(filepath.Join(repo, root), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if hasAllowedRoot(rel, directDatabaseAdapterRoots) {
				return nil
			}
			count, err := databaseCallCount(path)
			if err != nil {
				return err
			}
			if count > 0 {
				actual[rel] = count
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}

	var violations []string
	for path, count := range actual {
		allowed, known := directDatabaseAccessBaseline[path]
		if !known {
			violations = append(violations, path+": new Database() dependency")
			continue
		}
		if count > allowed {
			violations = append(violations, path+": Database() calls increased from "+strconv.Itoa(allowed)+" to "+strconv.Itoa(count))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("direct workflow database access crossed the architecture boundary:\n  %s\nadd a consumer-owned port and implement it in internal/persistence/sqlite instead", strings.Join(violations, "\n  "))
	}
}

func databaseCallCount(path string) (int, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return 0, err
	}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "Database" {
			count++
		}
		return true
	})
	return count, nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func hasAllowedRoot(path string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}
