package review

import (
	"errors"
	"testing"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/workflow"
)

func TestDeterministicRiskIsOrderedAndMonotonic(t *testing.T) {
	p := DefaultRiskPolicy()
	tests := []struct {
		name    string
		route   workflow.Route
		changes []Change
		want    Risk
		depth   Depth
	}{
		{"simple text", workflow.RouteSimple, []Change{{Path: "README.md", Kind: ChangeText}}, RiskLow, DepthDeterministic},
		{"planned floor", workflow.RoutePlanned, []Change{{Path: "README.md", Kind: ChangeText}}, RiskMedium, DepthOneLens},
		{"medium path", workflow.RouteSimple, []Change{{Path: "internal/x.go", Kind: ChangeText}}, RiskMedium, DepthOneLens},
		{"critical path", workflow.RouteSimple, []Change{{Path: ".github/workflows/ci.yml", Kind: ChangeText}}, RiskHigh, DepthFourLenses},
		{"dependency", workflow.RouteSimple, []Change{{Path: "go.mod", Kind: ChangeDependency}}, RiskHigh, DepthFourLenses},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DeterministicFloor(tc.route, tc.changes, p)
			if got.Risk != tc.want || got.Depth != tc.depth {
				t.Fatalf("floor=%#v", got)
			}
		})
	}
	if !(rank(RiskLow) < rank(RiskMedium) && rank(RiskMedium) < rank(RiskHigh)) {
		t.Fatal("risk ordering changed")
	}
}

func TestSemanticCannotLowerAndDepthOneRecordsLens(t *testing.T) {
	candidate, policy := testCandidate(t, "wf-1", "tree-1")
	floor := DeterministicFloor(workflow.RoutePlanned, nil, policy)
	base := SemanticInput{Justification: "bounded but non-trivial", ModelProvider: "provider", ModelID: "model", ModelVersion: "1", PromptTemplateID: "semantic-v1", RenderedRedactedPrompt: "redacted", EvidenceIDs: []string{"accept", "check"}}
	lowered := base
	lowered.RequestedRisk = RiskLow
	lowered.SelectedLens = LensReliability
	if _, err := AssessSemantic(candidate, floor, lowered, time.Now()); !errors.Is(err, ErrSemanticLowering) {
		t.Fatalf("lowering error=%v", err)
	}
	medium := base
	medium.RequestedRisk = RiskMedium
	medium.SelectedLens = LensReliability
	a, err := AssessSemantic(candidate, floor, medium, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.EffectiveRisk != RiskMedium || a.SelectedDepth != DepthOneLens || a.SelectedLens != LensReliability || a.PromptHash == "" {
		t.Fatalf("assessment=%#v", a)
	}
	high := base
	high.RequestedRisk = RiskHigh
	a, err = AssessSemantic(candidate, floor, high, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.EffectiveRisk != RiskHigh || a.SelectedDepth != DepthFourLenses {
		t.Fatalf("assessment=%#v", a)
	}
}

func TestReceiptAuthorityReplacementAndInvalidation(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	c1, p := testCandidate(t, "wf-1", "tree-1")
	r1 := issueMedium(t, store, c1, p, now)
	got, err := store.Authority("wf-1")
	if err != nil || got.ID != r1.ID {
		t.Fatalf("authority=%#v err=%v", got, err)
	}
	r1Again := issueMedium(t, store, c1, p, now.Add(time.Minute))
	if r1Again.ID != r1.ID {
		t.Fatalf("receipt changed: %s %s", r1.ID, r1Again.ID)
	}
	c2, _ := testCandidate(t, "wf-1", "tree-2")
	r2 := issueMedium(t, store, c2, p, now.Add(2*time.Minute))
	if r2.ID == r1.ID {
		t.Fatal("replacement reused receipt")
	}
	got, _ = store.Authority("wf-1")
	if got.ID != r2.ID {
		t.Fatalf("authority=%s", got.ID)
	}
	if _, err = store.Receipt(r1.ID); err != nil {
		t.Fatalf("historical receipt lost: %v", err)
	}
	if err = store.Invalidate(Invalidation{WorkflowID: "wf-1", CandidateRecordID: c2.ID, Reason: "candidate drift", OccurredAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Authority("wf-1"); !errors.Is(err, ErrNoAuthority) {
		t.Fatalf("authority survived drift: %v", err)
	}
	if _, err = store.Receipt(r2.ID); err != nil {
		t.Fatalf("invalidated receipt was not retained: %v", err)
	}
}

func TestReceiptRejectsCandidatePolicyAndEvidenceMismatch(t *testing.T) {
	c, p := testCandidate(t, "wf-1", "tree-1")
	floor := DeterministicFloor(workflow.RoutePlanned, nil, p)
	a, err := AssessSemantic(c, floor, semanticInput(RiskMedium, LensReliability), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	evidence := testEvidence(c, LensReliability)
	store := NewMemoryStore()
	wrongCandidate := a
	wrongCandidate.CandidateTreeOID = "other"
	if _, err = store.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: wrongCandidate, Evidence: evidence}); !errors.Is(err, ErrCandidateMismatch) {
		t.Fatalf("candidate error=%v", err)
	}
	wrongPolicy := a
	wrongPolicy.PolicyHash = "old-policy"
	if _, err = store.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: wrongPolicy, Evidence: evidence}); !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("policy error=%v", err)
	}
	wrongEvidence := append([]Evidence(nil), evidence...)
	wrongEvidence[0].CandidateTreeOID = "other"
	if _, err = store.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: a, Evidence: wrongEvidence}); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("evidence error=%v", err)
	}
	if _, err = store.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: a, Evidence: evidence[:2]}); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("lens evidence error=%v", err)
	}
}

func testCandidate(t *testing.T, workflowID, tree string) (CandidateRecord, RiskPolicy) {
	t.Helper()
	p := DefaultRiskPolicy()
	gc := gitcandidate.Candidate{TreeOID: tree, Seal: gitcandidate.ContextSeal{RepositoryRoot: "/repo", BaseCommitOID: "base", BaseTreeOID: "base-tree", ObjectFormat: "sha1"}, Manifest: []gitcandidate.ManifestEntry{{Path: "file.go", Mode: "100644", Kind: "blob", OID: "blob"}}}
	c, err := NewCandidateRecord(workflowID, gc, p.Hash(), "engine-v1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c, p
}
func semanticInput(r Risk, l Lens) SemanticInput {
	return SemanticInput{RequestedRisk: r, SelectedLens: l, Justification: "auditable reason", EvidenceIDs: []string{"accept", "check", "review"}, ModelProvider: "provider", ModelID: "model", ModelVersion: "1", PromptTemplateID: "semantic-v1", RenderedRedactedPrompt: "redacted prompt"}
}
func testEvidence(c CandidateRecord, l Lens) []Evidence {
	return []Evidence{{ID: "accept", Kind: EvidenceAcceptance, CandidateRecordID: c.ID, CandidateTreeOID: c.TreeOID, PolicyHash: c.PolicyHash, Digest: "a"}, {ID: "check", Kind: EvidenceCheck, CandidateRecordID: c.ID, CandidateTreeOID: c.TreeOID, PolicyHash: c.PolicyHash, Digest: "c"}, {ID: "review", Kind: EvidenceReview, CandidateRecordID: c.ID, CandidateTreeOID: c.TreeOID, PolicyHash: c.PolicyHash, Digest: "r", Lens: l}}
}
func issueMedium(t *testing.T, s *MemoryStore, c CandidateRecord, p RiskPolicy, now time.Time) Receipt {
	t.Helper()
	floor := DeterministicFloor(workflow.RoutePlanned, nil, p)
	a, err := AssessSemantic(c, floor, semanticInput(RiskMedium, LensReliability), now)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Issue(IssueRequest{Candidate: c, Floor: floor, Assessment: a, Evidence: testEvidence(c, LensReliability), IssuedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
