package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestWorkflowCommandRegistryContract(t *testing.T) {
	want := []string{
		"start", "run", "notifications", "review", "deliver", "status",
		"inspect", "receipt", "approve", "revoke-approval", "abort",
		"resume", "retry-verification", "replan", "export", "frontier",
		"answer", "close-discovery", "worker",
	}
	if len(workflowCommands) != len(want) {
		t.Fatalf("registered commands = %d, want %d", len(workflowCommands), len(want))
	}

	var general bytes.Buffer
	printWorkflowUsage(&general)
	seen := map[string]bool{}
	for index, spec := range workflowCommands {
		if spec.Name != want[index] {
			t.Errorf("command[%d] = %q, want %q", index, spec.Name, want[index])
		}
		if seen[spec.Name] {
			t.Errorf("duplicate command %q", spec.Name)
		}
		seen[spec.Name] = true
		if spec.Name == "" || spec.Usage == "" {
			t.Errorf("incomplete command spec: %#v", spec)
		}
		if !workflowCommandKnown(spec.Name) {
			t.Errorf("registered command %q is not recognized", spec.Name)
		}

		var commandHelp bytes.Buffer
		if err := printWorkflowCommandUsage(spec.Name, &commandHelp); err != nil {
			t.Errorf("help for %q: %v", spec.Name, err)
		} else if strings.TrimSpace(commandHelp.String()) != strings.TrimSpace(spec.Usage) {
			t.Errorf("help for %q drifted from its registry entry", spec.Name)
		}

		if spec.Hidden {
			if strings.Contains(general.String(), "  "+spec.Name+" ") {
				t.Errorf("hidden command %q leaked into general help", spec.Name)
			}
			continue
		}
		if spec.Summary == "" {
			t.Errorf("public command %q has no summary", spec.Name)
		}
		if !strings.Contains(general.String(), spec.Name) || !strings.Contains(general.String(), spec.Summary) {
			t.Errorf("general help missing registered command %q", spec.Name)
		}
	}

	if workflowCommandKnown("not-a-command") {
		t.Fatal("unknown command was accepted")
	}
	if err := printWorkflowCommandUsage("not-a-command", &bytes.Buffer{}); err == nil {
		t.Fatal("unknown command returned help")
	}
}

func TestWorkflowCommandRegistryMatchesDispatch(t *testing.T) {
	registered := make([]string, 0, len(workflowCommands))
	for _, spec := range workflowCommands {
		registered = append(registered, spec.Name)
	}
	dispatched := workflowDispatchCases(t)
	sort.Strings(registered)
	sort.Strings(dispatched)
	if !reflect.DeepEqual(dispatched, registered) {
		t.Fatalf("workflow command registry and dispatch differ\n registered: %v\n dispatched: %v", registered, dispatched)
	}
}

func workflowDispatchCases(t *testing.T) []string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve workflow registry test path")
	}
	path := filepath.Join(filepath.Dir(testFile), "workflow.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "runWorkflowCLI" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switchStatement, ok := node.(*ast.SwitchStmt)
			if !ok || !isArgsZero(switchStatement.Tag) {
				return true
			}
			for _, statement := range switchStatement.Body.List {
				clause, ok := statement.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expression := range clause.List {
					literal, ok := expression.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					commands = append(commands, value)
				}
			}
			return false
		})
	}
	if len(commands) == 0 {
		t.Fatal("runWorkflowCLI dispatch switch was not found")
	}
	return commands
}

func isArgsZero(expression ast.Expr) bool {
	index, ok := expression.(*ast.IndexExpr)
	if !ok {
		return false
	}
	identifier, ok := index.X.(*ast.Ident)
	if !ok || identifier.Name != "args" {
		return false
	}
	literal, ok := index.Index.(*ast.BasicLit)
	return ok && literal.Kind == token.INT && literal.Value == "0"
}
