package orchestration

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/review"
	"github.com/joeldevz/skynex/internal/workflow"
)

type NodeType string

const (
	NodeDecision  NodeType = "decision"
	NodeFog       NodeType = "fog"
	NodeResearch  NodeType = "research"
	NodePrototype NodeType = "prototype"
	NodeGrill     NodeType = "grill"
)

type RelationType string

const (
	RelationDependsOn   RelationType = "depends_on"
	RelationUnblocks    RelationType = "unblocks"
	RelationInvalidates RelationType = "invalidates"
	RelationDerivedFrom RelationType = "derived_from"
)

type WayfinderNode struct {
	ID       string   `json:"id"`
	Type     NodeType `json:"type"`
	Question string   `json:"question,omitempty"`
	Blocking bool     `json:"blocking"`
	Resolved bool     `json:"resolved"`
	Unlocks  []string `json:"unlocks,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Actor    string   `json:"actor,omitempty"`
}
type Relation struct {
	From, To string
	Type     RelationType
}
type WayfinderGraph struct {
	WorkflowID string          `json:"workflow_id"`
	Version    uint64          `json:"version"`
	Nodes      []WayfinderNode `json:"nodes"`
	Relations  []Relation      `json:"relations"`
	Closed     bool            `json:"closed"`
}
type Slice struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Dependencies       []string `json:"dependencies,omitempty"`
}
type ExecutionGraph struct {
	WorkflowID string  `json:"workflow_id"`
	Version    uint64  `json:"version"`
	Slices     []Slice `json:"slices"`
}
type ExecutableContract struct {
	Destination         string   `json:"destination"`
	AcceptanceCriteria  []string `json:"acceptance_criteria"`
	Assumptions         []string `json:"assumptions,omitempty"`
	Risks               []string `json:"risks,omitempty"`
	BlockingDecisionIDs []string `json:"blocking_decision_ids,omitempty"`
}

type RouteInput struct {
	Clear               bool
	EstimatedSlices     int
	BlockingUncertainty []string
}
type RouteOverride struct {
	Route         workflow.Route
	Actor, Reason string
	At            time.Time
}
type RouteDecision struct {
	Route       workflow.Route `json:"route"`
	MinimumRisk review.Risk    `json:"minimum_risk"`
	Rationale   string         `json:"rationale"`
	Override    *RouteOverride `json:"override,omitempty"`
}

func SelectRoute(input RouteInput, override *RouteOverride) RouteDecision {
	route := workflow.RouteDiscovery
	rationale := "blocking uncertainty requires discovery"
	if len(input.BlockingUncertainty) == 0 && input.Clear {
		if input.EstimatedSlices <= 1 {
			route = workflow.RouteSimple
			rationale = "clear bounded request fits one production slice"
		} else {
			route = workflow.RoutePlanned
			rationale = "clear request requires multiple production slices"
		}
	}
	floor := routeFloor(route)
	decision := RouteDecision{Route: route, MinimumRisk: floor, Rationale: rationale}
	if override != nil && override.Actor != "" && override.Reason != "" {
		copy := *override
		decision.Override = &copy
		decision.Route = copy.Route
		decision.MinimumRisk = maxRisk(floor, routeFloor(copy.Route))
		decision.Rationale = "manual route override: " + copy.Reason
	}
	return decision
}
func routeFloor(route workflow.Route) review.Risk {
	if route == workflow.RouteSimple {
		return review.RiskLow
	}
	return review.RiskMedium
}
func maxRisk(a, b review.Risk) review.Risk {
	order := map[review.Risk]int{review.RiskLow: 0, review.RiskMedium: 1, review.RiskHigh: 2}
	if order[b] > order[a] {
		return b
	}
	return a
}

func (g WayfinderGraph) Frontier() (*WayfinderNode, error) {
	var candidates []WayfinderNode
	for _, n := range g.Nodes {
		if n.Type == NodeGrill && n.Blocking && !n.Resolved && n.Question != "" {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i].Unlocks) == len(candidates[j].Unlocks) {
			return candidates[i].ID < candidates[j].ID
		}
		return len(candidates[i].Unlocks) > len(candidates[j].Unlocks)
	})
	chosen := candidates[0]
	return &chosen, nil
}
func ValidateWayfinder(g WayfinderGraph) error {
	ids := map[string]bool{}
	for _, n := range g.Nodes {
		if !strings.HasPrefix(n.ID, "wfnode_") {
			return errors.New("orchestration: Wayfinder node IDs must use wfnode_ prefix")
		}
		if ids[n.ID] {
			return errors.New("orchestration: duplicate Wayfinder node ID")
		}
		ids[n.ID] = true
		switch n.Type {
		case NodeDecision, NodeFog, NodeResearch, NodePrototype, NodeGrill:
		default:
			return errors.New("orchestration: invalid Wayfinder node type")
		}
	}
	for _, r := range g.Relations {
		if !ids[r.From] || !ids[r.To] {
			return errors.New("orchestration: Wayfinder relation references unknown node")
		}
	}
	return nil
}
func ValidateContract(c ExecutableContract) error {
	if strings.TrimSpace(c.Destination) == "" || len(c.AcceptanceCriteria) == 0 {
		return errors.New("orchestration: executable contract needs destination and acceptance criteria")
	}
	for _, criterion := range c.AcceptanceCriteria {
		if strings.TrimSpace(criterion) == "" {
			return errors.New("orchestration: empty acceptance criterion")
		}
	}
	if len(c.BlockingDecisionIDs) > 0 {
		return errors.New("orchestration: executable contract retains blocking decisions")
	}
	return nil
}
func ValidateExecution(g ExecutionGraph) error {
	if len(g.Slices) == 0 {
		return errors.New("orchestration: execution graph has no vertical slices")
	}
	ids := map[string]bool{}
	for _, s := range g.Slices {
		if !strings.HasPrefix(s.ID, "slice_") {
			return errors.New("orchestration: execution IDs must use slice_ prefix")
		}
		if ids[s.ID] || s.Title == "" || len(s.AcceptanceCriteria) == 0 {
			return errors.New("orchestration: invalid or duplicate vertical slice")
		}
		ids[s.ID] = true
	}
	indegree := map[string]int{}
	edges := map[string][]string{}
	for _, s := range g.Slices {
		for _, dep := range s.Dependencies {
			if !ids[dep] || dep == s.ID {
				return errors.New("orchestration: invalid slice dependency")
			}
			edges[dep] = append(edges[dep], s.ID)
			indegree[s.ID]++
		}
	}
	var queue []string
	for id := range ids {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, next := range edges[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if seen != len(ids) {
		return errors.New("orchestration: execution graph contains a cycle")
	}
	return nil
}
func ValidateGraphSeparation(w WayfinderGraph, e ExecutionGraph) error {
	ids := map[string]bool{}
	for _, n := range w.Nodes {
		ids[n.ID] = true
	}
	for _, s := range e.Slices {
		if ids[s.ID] {
			return errors.New("orchestration: graph node IDs overlap")
		}
	}
	return nil
}

type Engine struct {
	workflows *workflow.SQLiteStore
	db        *sql.DB
}

func NewEngine(store *workflow.SQLiteStore) *Engine {
	return &Engine{workflows: store, db: store.Database()}
}
func (e *Engine) Begin(workflowID string, input RouteInput, override *RouteOverride) (RouteDecision, error) {
	decision := SelectRoute(input, override)
	raw, _ := json.Marshal(decision)
	if err := insertImmutable(e.db, `INSERT OR IGNORE INTO orchestration_routes(workflow_id,decision) VALUES(?,?)`, `SELECT decision FROM orchestration_routes WHERE workflow_id=?`, workflowID, raw); err != nil {
		return RouteDecision{}, err
	}
	if _, err := e.db.Exec(`UPDATE workflows SET route=?,minimum_risk=? WHERE id=? AND state=? AND state_version=0`, decision.Route, decision.MinimumRisk, workflowID, workflow.StateCreated); err != nil {
		return RouteDecision{}, err
	}
	graph := WayfinderGraph{WorkflowID: workflowID, Version: 1, Nodes: nil, Closed: false}
	if decision.Route == workflow.RouteDiscovery {
		for i, item := range input.BlockingUncertainty {
			graph.Nodes = append(graph.Nodes, WayfinderNode{ID: fmt.Sprintf("wfnode_fog_%d", i+1), Type: NodeFog, Question: item, Blocking: true})
		}
	}
	if err := e.saveWayfinder(graph); err != nil {
		return RouteDecision{}, err
	}
	_, err := e.workflows.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: workflow.StateCreated, ExpectedVersion: 0, NextState: workflow.StateDiscovering, IdempotencyKey: "orchestration:discover:v1", ArtifactIDs: []string{fmt.Sprintf("wayfinder:%d", graph.Version)}})
	return decision, err
}
func (e *Engine) Close(workflowID string, graph WayfinderGraph, contract ExecutableContract, execution ExecutionGraph) error {
	if graph.WorkflowID != workflowID || execution.WorkflowID != workflowID {
		return errors.New("orchestration: graph workflow identity mismatch")
	}
	if err := ValidateWayfinder(graph); err != nil {
		return err
	}
	if frontier, _ := graph.Frontier(); frontier != nil {
		return errors.New("orchestration: blocking Grill question remains")
	}
	for _, n := range graph.Nodes {
		if n.Blocking && !n.Resolved {
			return errors.New("orchestration: blocking discovery node remains")
		}
	}
	if err := ValidateContract(contract); err != nil {
		return err
	}
	if err := ValidateExecution(execution); err != nil {
		return err
	}
	if err := ValidateGraphSeparation(graph, execution); err != nil {
		return err
	}
	graph.Closed = true
	graph.Version++
	if err := e.saveWayfinder(graph); err != nil {
		return err
	}
	contractRaw, _ := json.Marshal(contract)
	if err := insertImmutable(e.db, `INSERT OR IGNORE INTO execution_contracts(workflow_id,contract) VALUES(?,?)`, `SELECT contract FROM execution_contracts WHERE workflow_id=?`, workflowID, contractRaw); err != nil {
		return err
	}
	executionRaw, _ := json.Marshal(execution)
	if err := insertImmutable(e.db, `INSERT OR IGNORE INTO execution_graphs(workflow_id,version,graph) VALUES(?,?,?)`, `SELECT graph FROM execution_graphs WHERE workflow_id=? AND version=?`, workflowID, executionRaw, execution.Version); err != nil {
		return err
	}
	_, err := e.workflows.Transition(workflow.Transition{WorkflowID: workflowID, ExpectedState: workflow.StateDiscovering, ExpectedVersion: 1, NextState: workflow.StateReady, IdempotencyKey: "orchestration:ready:v1", ArtifactIDs: []string{"contract:v1", fmt.Sprintf("execution:%d", execution.Version)}})
	return err
}
func (e *Engine) saveWayfinder(g WayfinderGraph) error {
	raw, _ := json.Marshal(g)
	return insertImmutable(e.db, `INSERT OR IGNORE INTO wayfinder_graphs(workflow_id,version,graph) VALUES(?,?,?)`, `SELECT graph FROM wayfinder_graphs WHERE workflow_id=? AND version=?`, g.WorkflowID, raw, g.Version)
}
func insertImmutable(db *sql.DB, insert, query string, key string, raw []byte, extra ...any) error {
	args := []any{key}
	if strings.Contains(insert, "version,graph") {
		args = append(args, extra[0])
	}
	args = append(args, raw)
	if _, err := db.Exec(insert, args...); err != nil {
		return err
	}
	queryArgs := []any{key}
	if len(extra) > 0 {
		queryArgs = append(queryArgs, extra[0])
	}
	var existing []byte
	if err := db.QueryRow(query, queryArgs...).Scan(&existing); err != nil {
		return err
	}
	if string(existing) != string(raw) {
		return errors.New("orchestration: immutable artifact conflict")
	}
	return nil
}
