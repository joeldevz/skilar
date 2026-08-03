package orchestration

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

func TestRouteSelectionAndOverrideCannotLowerRisk(t *testing.T) {
	tests := []struct {
		input RouteInput
		want  workflow.Route
	}{{RouteInput{Clear: true, EstimatedSlices: 1}, workflow.RouteSimple}, {RouteInput{Clear: true, EstimatedSlices: 3}, workflow.RoutePlanned}, {RouteInput{Clear: false}, workflow.RouteDiscovery}, {RouteInput{Clear: true, EstimatedSlices: 1, BlockingUncertainty: []string{"unknown"}}, workflow.RouteDiscovery}}
	for _, tc := range tests {
		if got := SelectRoute(tc.input, nil); got.Route != tc.want {
			t.Fatalf("route=%s want=%s", got.Route, tc.want)
		}
	}
	override := RouteOverride{Route: workflow.RouteSimple, Actor: "operator", Reason: "bounded after investigation", At: time.Now()}
	got := SelectRoute(RouteInput{BlockingUncertainty: []string{"unknown"}}, &override)
	if got.Route != workflow.RouteSimple || got.MinimumRisk != review.RiskMedium || got.Override == nil {
		t.Fatalf("override=%#v", got)
	}
}

func TestGraphIDSeparationDAGAndCycles(t *testing.T) {
	way := WayfinderGraph{Nodes: []WayfinderNode{{ID: "wfnode_1", Type: NodeDecision, Resolved: true}}}
	valid := ExecutionGraph{Slices: []Slice{{ID: "slice_a", Title: "A", AcceptanceCriteria: []string{"works"}}, {ID: "slice_b", Title: "B", AcceptanceCriteria: []string{"works"}, Dependencies: []string{"slice_a"}}}}
	if err := ValidateWayfinder(way); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecution(valid); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGraphSeparation(way, valid); err != nil {
		t.Fatal(err)
	}
	cycle := ExecutionGraph{Slices: []Slice{{ID: "slice_a", Title: "A", AcceptanceCriteria: []string{"a"}, Dependencies: []string{"slice_b"}}, {ID: "slice_b", Title: "B", AcceptanceCriteria: []string{"b"}, Dependencies: []string{"slice_a"}}}}
	if err := ValidateExecution(cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error=%v", err)
	}
	if err := ValidateWayfinder(WayfinderGraph{Nodes: []WayfinderNode{{ID: "slice_a", Type: NodeDecision}}}); err == nil {
		t.Fatal("execution ID accepted by Wayfinder")
	}
}

func TestFrontierChoosesOneHighestUnlockGrill(t *testing.T) {
	g := WayfinderGraph{Nodes: []WayfinderNode{
		{ID: "wfnode_g1", Type: NodeGrill, Question: "small?", Blocking: true, Unlocks: []string{"a"}},
		{ID: "wfnode_g2", Type: NodeGrill, Question: "largest?", Blocking: true, Unlocks: []string{"a", "b", "c"}},
		{ID: "wfnode_g3", Type: NodeGrill, Question: "answered", Blocking: true, Resolved: true, Unlocks: []string{"a", "b", "c", "d"}},
	}}
	frontier, err := g.Frontier()
	if err != nil || frontier == nil || frontier.ID != "wfnode_g2" {
		t.Fatalf("frontier=%#v err=%v", frontier, err)
	}
}

func TestZeroNodeClosureIsDurableAndIdempotent(t *testing.T) {
	store, err := workflow.OpenSQLite(filepath.Join(t.TempDir(), "state", "workflows.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.Create(workflow.Workflow{ID: "wf-zero", Route: workflow.RouteSimple, MinimumRisk: workflow.RiskLow, BasisTree: "tree"}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store)
	input := RouteInput{Clear: true, EstimatedSlices: 1}
	decision, err := engine.Begin("wf-zero", input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Route != workflow.RouteSimple {
		t.Fatalf("decision=%#v", decision)
	}
	if _, err = engine.Begin("wf-zero", input, nil); err != nil {
		t.Fatalf("begin replay=%v", err)
	}
	graph := WayfinderGraph{WorkflowID: "wf-zero", Version: 1}
	contract := ExecutableContract{Destination: "feature complete", AcceptanceCriteria: []string{"behavior verified"}}
	execution := ExecutionGraph{WorkflowID: "wf-zero", Version: 1, Slices: []Slice{
		{ID: "slice_feature", Title: "Feature", AcceptanceCriteria: []string{"behavior verified"}},
	}}
	if err = engine.Close("wf-zero", graph, contract, execution); err != nil {
		t.Fatal(err)
	}
	if err = engine.Close("wf-zero", graph, contract, execution); err != nil {
		t.Fatalf("close replay=%v", err)
	}
	w, err := store.Get("wf-zero")
	if err != nil || w.State != workflow.StateReady || w.StateVersion != 2 {
		t.Fatalf("workflow=%#v err=%v", w, err)
	}
	var raw []byte
	if err = store.Database().QueryRow(`SELECT graph FROM wayfinder_graphs WHERE workflow_id=? AND version=2`, "wf-zero").Scan(&raw); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidContractAndBlockingDiscovery(t *testing.T) {
	if err := ValidateContract(ExecutableContract{Destination: "x"}); err == nil {
		t.Fatal("accepted unverifiable contract")
	}
	g := WayfinderGraph{Nodes: []WayfinderNode{{ID: "wfnode_grill", Type: NodeGrill, Question: "choose", Blocking: true}}}
	if frontier, _ := g.Frontier(); frontier == nil {
		t.Fatal("missing blocking frontier")
	}
}
