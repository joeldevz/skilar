package review

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"
)

type Receipt struct {
	ID                string
	WorkflowID        string
	CandidateRecordID string
	CandidateTreeOID  string
	PolicyHash        string
	EngineVersion     string
	EffectiveRisk     Risk
	Depth             Depth
	Lens              Lens
	EvidenceIDs       []string
	EvidenceSetDigest string
	AssessmentDigest  string
	IssuedAt          time.Time
}
type IssueRequest struct {
	Candidate  CandidateRecord
	Floor      RiskFloor
	Assessment SemanticAssessment
	Evidence   []Evidence
	IssuedAt   time.Time
}
type Invalidation struct {
	WorkflowID, CandidateRecordID, Reason string
	OccurredAt                            time.Time
}
type Store interface {
	Issue(IssueRequest) (Receipt, error)
	Receipt(string) (Receipt, error)
	Authority(string) (Receipt, error)
	Invalidate(Invalidation) error
}

type MemoryStore struct {
	mu            sync.Mutex
	receipts      map[string]Receipt
	byCandidate   map[string]string
	authority     map[string]string
	invalidations []Invalidation
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{receipts: map[string]Receipt{}, byCandidate: map[string]string{}, authority: map[string]string{}}
}

func (s *MemoryStore) Issue(req IssueRequest) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := req.Candidate
	a := req.Assessment
	if err := ValidateCandidateRecord(c); err != nil {
		return Receipt{}, err
	}
	if req.Floor.PolicyHash != c.PolicyHash || a.MinimumRisk != req.Floor.Risk || req.Floor.Depth != DepthFor(req.Floor.Risk) {
		return Receipt{}, ErrPolicyMismatch
	}
	if a.CandidateRecordID != c.ID || a.CandidateTreeOID != c.TreeOID {
		return Receipt{}, ErrCandidateMismatch
	}
	if a.PolicyHash != c.PolicyHash {
		return Receipt{}, ErrPolicyMismatch
	}
	if a.PromptHash != digest([]byte(a.RenderedRedactedPrompt)) || a.ModelProvider == "" || a.ModelID == "" || a.PromptTemplateID == "" || a.Justification == "" {
		return Receipt{}, errors.New("review: invalid semantic audit")
	}
	if a.EffectiveRisk != maxRisk(a.MinimumRisk, a.RequestedRisk) || a.SelectedDepth != DepthFor(a.EffectiveRisk) {
		return Receipt{}, ErrSemanticLowering
	}
	if a.SelectedDepth == DepthOneLens && !validLens(a.SelectedLens) {
		return Receipt{}, errors.New("review: depth 1 receipt requires a lens")
	}
	if err := ValidateEvidence(c, req.Evidence); err != nil {
		return Receipt{}, err
	}
	evidence := append([]Evidence(nil), req.Evidence...)
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].ID < evidence[j].ID })
	ids := make([]string, 0, len(evidence))
	kinds := map[EvidenceKind]int{}
	known := map[string]bool{}
	lenses := map[Lens]bool{}
	for _, e := range evidence {
		if known[e.ID] {
			return Receipt{}, ErrEvidenceMismatch
		}
		ids = append(ids, e.ID)
		kinds[e.Kind]++
		known[e.ID] = true
		if e.Kind == EvidenceReview && validLens(e.Lens) {
			lenses[e.Lens] = true
		}
	}
	if kinds[EvidenceAcceptance] == 0 || kinds[EvidenceCheck] == 0 {
		return Receipt{}, ErrEvidenceMismatch
	}
	if a.SelectedDepth == DepthOneLens && !lenses[a.SelectedLens] {
		return Receipt{}, ErrEvidenceMismatch
	}
	if a.SelectedDepth == DepthFourLenses && !(lenses[LensRisk] && lenses[LensReadability] && lenses[LensReliability] && lenses[LensResilience]) {
		return Receipt{}, ErrEvidenceMismatch
	}
	for _, id := range a.EvidenceIDs {
		if !known[id] {
			return Receipt{}, ErrEvidenceMismatch
		}
	}
	raw, _ := json.Marshal(evidence)
	evidenceDigest := digest(raw)
	assessmentForDigest := a
	assessmentForDigest.AssessedAt = time.Time{}
	assessmentRaw, _ := json.Marshal(assessmentForDigest)
	assessmentDigest := digest(assessmentRaw)
	basis := struct {
		CandidateID, Tree, Policy, Engine string
		Risk                              Risk
		Depth                             Depth
		Lens                              Lens
		EvidenceDigest                    string
		AssessmentDigest                  string
	}{c.ID, c.TreeOID, c.PolicyHash, c.EngineVersion, a.EffectiveRisk, a.SelectedDepth, a.SelectedLens, evidenceDigest, assessmentDigest}
	basisJSON, _ := json.Marshal(basis)
	receipt := Receipt{ID: "rcpt_" + digest(basisJSON), WorkflowID: c.WorkflowID, CandidateRecordID: c.ID, CandidateTreeOID: c.TreeOID, PolicyHash: c.PolicyHash, EngineVersion: c.EngineVersion, EffectiveRisk: a.EffectiveRisk, Depth: a.SelectedDepth, Lens: a.SelectedLens, EvidenceIDs: ids, EvidenceSetDigest: evidenceDigest, AssessmentDigest: assessmentDigest, IssuedAt: req.IssuedAt.UTC()}
	if existingID, ok := s.byCandidate[c.ID]; ok {
		existing := s.receipts[existingID]
		if existing.ID == receipt.ID {
			return cloneReceipt(existing), nil
		}
		return Receipt{}, ErrReceiptExists
	}
	s.receipts[receipt.ID] = cloneReceipt(receipt)
	s.byCandidate[c.ID] = receipt.ID
	s.authority[c.WorkflowID] = receipt.ID
	return cloneReceipt(receipt), nil
}
func (s *MemoryStore) Receipt(id string) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[id]
	if !ok {
		return Receipt{}, ErrNoAuthority
	}
	return cloneReceipt(r), nil
}
func (s *MemoryStore) Authority(workflowID string) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.authority[workflowID]
	if !ok {
		return Receipt{}, ErrNoAuthority
	}
	return cloneReceipt(s.receipts[id]), nil
}
func (s *MemoryStore) Invalidate(i Invalidation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.authority[i.WorkflowID]
	if !ok {
		return ErrNoAuthority
	}
	r := s.receipts[id]
	if r.CandidateRecordID != i.CandidateRecordID {
		return ErrCandidateMismatch
	}
	delete(s.authority, i.WorkflowID)
	s.invalidations = append(s.invalidations, i)
	return nil
}
func cloneReceipt(r Receipt) Receipt {
	r.EvidenceIDs = append([]string(nil), r.EvidenceIDs...)
	return r
}
