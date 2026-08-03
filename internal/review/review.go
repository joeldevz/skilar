package review

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joeldevz/skynex/internal/gitcandidate"
	"github.com/joeldevz/skynex/internal/workflow"
)

type Risk string

const (
	RiskLow    Risk = "low"
	RiskMedium Risk = "medium"
	RiskHigh   Risk = "high"
)

type Depth int

const (
	DepthDeterministic Depth = 0
	DepthOneLens       Depth = 1
	DepthFourLenses    Depth = 4
)

type Lens string

const (
	LensRisk        Lens = "risk"
	LensReadability Lens = "readability"
	LensReliability Lens = "reliability"
	LensResilience  Lens = "resilience"
)

var (
	ErrSemanticLowering  = errors.New("review: semantic assessment cannot lower deterministic risk")
	ErrCandidateMismatch = errors.New("review: candidate mismatch")
	ErrPolicyMismatch    = errors.New("review: policy mismatch")
	ErrEvidenceMismatch  = errors.New("review: evidence basis mismatch")
	ErrReceiptExists     = errors.New("review: immutable receipt already exists for candidate")
	ErrNoAuthority       = errors.New("review: no authoritative receipt")
)

func rank(r Risk) int {
	switch r {
	case RiskLow:
		return 0
	case RiskMedium:
		return 1
	case RiskHigh:
		return 2
	}
	return -1
}
func DepthFor(r Risk) Depth {
	switch r {
	case RiskHigh:
		return DepthFourLenses
	case RiskMedium:
		return DepthOneLens
	default:
		return DepthDeterministic
	}
}
func maxRisk(a, b Risk) Risk {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

type ChangeKind string

const (
	ChangeText       ChangeKind = "text"
	ChangeBinary     ChangeKind = "binary"
	ChangeDependency ChangeKind = "dependency"
	ChangeSchema     ChangeKind = "schema"
	ChangePublicAPI  ChangeKind = "public_api"
	ChangeGenerated  ChangeKind = "generated"
)

type Change struct {
	Path string
	Kind ChangeKind
}

type RiskPolicy struct {
	Version            string
	HighRiskPrefixes   []string
	MediumRiskPrefixes []string
	PlannedFloor       Risk
	DiscoveryFloor     Risk
	MediumFileCount    int
}

func DefaultRiskPolicy() RiskPolicy {
	return RiskPolicy{Version: "v1", HighRiskPrefixes: []string{".github/workflows/", "migrations/", "schemas/", "auth/", "security/"}, MediumRiskPrefixes: []string{"internal/", "cmd/"}, PlannedFloor: RiskMedium, DiscoveryFloor: RiskMedium, MediumFileCount: 20}
}
func (p RiskPolicy) Hash() string {
	copyP := p
	copyP.HighRiskPrefixes = append([]string(nil), p.HighRiskPrefixes...)
	copyP.MediumRiskPrefixes = append([]string(nil), p.MediumRiskPrefixes...)
	sort.Strings(copyP.HighRiskPrefixes)
	sort.Strings(copyP.MediumRiskPrefixes)
	raw, _ := json.Marshal(copyP)
	return digest(raw)
}

type RiskFloor struct {
	Risk       Risk
	Depth      Depth
	PolicyHash string
	Reasons    []string
}

func DeterministicFloor(route workflow.Route, changes []Change, policy RiskPolicy) RiskFloor {
	risk := RiskLow
	var reasons []string
	if route == workflow.RoutePlanned {
		risk = maxRisk(risk, orDefault(policy.PlannedFloor, RiskMedium))
		reasons = append(reasons, "planned route floor")
	}
	if route == workflow.RouteDiscovery {
		risk = maxRisk(risk, orDefault(policy.DiscoveryFloor, RiskMedium))
		reasons = append(reasons, "discovery route floor")
	}
	if policy.MediumFileCount > 0 && len(changes) >= policy.MediumFileCount {
		risk = maxRisk(risk, RiskMedium)
		reasons = append(reasons, "change count threshold")
	}
	for _, change := range changes {
		switch change.Kind {
		case ChangeBinary, ChangeDependency, ChangeSchema, ChangePublicAPI:
			risk = RiskHigh
			reasons = append(reasons, string(change.Kind)+" change")
		case ChangeGenerated:
			risk = maxRisk(risk, RiskMedium)
			reasons = append(reasons, "generated change")
		}
		for _, prefix := range policy.HighRiskPrefixes {
			if strings.HasPrefix(change.Path, prefix) {
				risk = RiskHigh
				reasons = append(reasons, "high-risk path: "+prefix)
			}
		}
		for _, prefix := range policy.MediumRiskPrefixes {
			if strings.HasPrefix(change.Path, prefix) {
				risk = maxRisk(risk, RiskMedium)
				reasons = append(reasons, "medium-risk path: "+prefix)
			}
		}
	}
	return RiskFloor{Risk: risk, Depth: DepthFor(risk), PolicyHash: policy.Hash(), Reasons: dedupe(reasons)}
}
func orDefault(value, fallback Risk) Risk {
	if rank(value) < 0 {
		return fallback
	}
	return value
}

type SemanticAssessment struct {
	CandidateRecordID      string
	CandidateTreeOID       string
	PolicyHash             string
	MinimumRisk            Risk
	RequestedRisk          Risk
	EffectiveRisk          Risk
	SelectedDepth          Depth
	SelectedLens           Lens
	Justification          string
	EvidenceIDs            []string
	ModelProvider          string
	ModelID                string
	ModelVersion           string
	PromptTemplateID       string
	RenderedRedactedPrompt string
	PromptHash             string
	AssessedAt             time.Time
}

type SemanticInput struct {
	RequestedRisk                                                                  Risk
	SelectedLens                                                                   Lens
	Justification                                                                  string
	EvidenceIDs                                                                    []string
	ModelProvider, ModelID, ModelVersion, PromptTemplateID, RenderedRedactedPrompt string
}

func AssessSemantic(candidate CandidateRecord, floor RiskFloor, input SemanticInput, now time.Time) (SemanticAssessment, error) {
	if floor.PolicyHash != candidate.PolicyHash {
		return SemanticAssessment{}, ErrPolicyMismatch
	}
	if rank(input.RequestedRisk) < rank(floor.Risk) {
		return SemanticAssessment{}, ErrSemanticLowering
	}
	effective := maxRisk(floor.Risk, input.RequestedRisk)
	depth := DepthFor(effective)
	if input.Justification == "" || input.ModelProvider == "" || input.ModelID == "" || input.PromptTemplateID == "" || input.RenderedRedactedPrompt == "" {
		return SemanticAssessment{}, errors.New("review: incomplete semantic audit")
	}
	if depth == DepthOneLens && !validLens(input.SelectedLens) {
		return SemanticAssessment{}, errors.New("review: depth 1 requires one valid lens")
	}
	if depth != DepthOneLens && input.SelectedLens != "" {
		return SemanticAssessment{}, errors.New("review: selected lens is only valid at depth 1")
	}
	return SemanticAssessment{CandidateRecordID: candidate.ID, CandidateTreeOID: candidate.TreeOID, PolicyHash: candidate.PolicyHash, MinimumRisk: floor.Risk, RequestedRisk: input.RequestedRisk, EffectiveRisk: effective, SelectedDepth: depth, SelectedLens: input.SelectedLens, Justification: input.Justification, EvidenceIDs: append([]string(nil), input.EvidenceIDs...), ModelProvider: input.ModelProvider, ModelID: input.ModelID, ModelVersion: input.ModelVersion, PromptTemplateID: input.PromptTemplateID, RenderedRedactedPrompt: input.RenderedRedactedPrompt, PromptHash: digest([]byte(input.RenderedRedactedPrompt)), AssessedAt: now.UTC()}, nil
}
func validLens(l Lens) bool {
	return l == LensRisk || l == LensReadability || l == LensReliability || l == LensResilience
}

type CandidateRecord struct {
	ID            string
	WorkflowID    string
	TreeOID       string
	Manifest      []gitcandidate.ManifestEntry
	Seal          gitcandidate.ContextSeal
	PolicyHash    string
	EngineVersion string
	FrozenAt      time.Time
}

func NewCandidateRecord(workflowID string, c gitcandidate.Candidate, policyHash, engineVersion string, now time.Time) (CandidateRecord, error) {
	if workflowID == "" || c.TreeOID == "" || policyHash == "" || engineVersion == "" {
		return CandidateRecord{}, errors.New("review: incomplete candidate record")
	}
	manifest := append([]gitcandidate.ManifestEntry(nil), c.Manifest...)
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	id := candidateRecordID(workflowID, c.TreeOID, policyHash, engineVersion, c.Seal, manifest)
	return CandidateRecord{ID: id, WorkflowID: workflowID, TreeOID: c.TreeOID, Manifest: manifest, Seal: c.Seal, PolicyHash: policyHash, EngineVersion: engineVersion, FrozenAt: now.UTC()}, nil
}

func candidateRecordID(workflowID, treeOID, policyHash, engineVersion string, seal gitcandidate.ContextSeal, manifest []gitcandidate.ManifestEntry) string {
	basis := struct {
		WorkflowID, TreeOID, PolicyHash, EngineVersion string
		Seal                                           gitcandidate.ContextSeal
		Manifest                                       []gitcandidate.ManifestEntry
	}{workflowID, treeOID, policyHash, engineVersion, seal, manifest}
	raw, _ := json.Marshal(basis)
	return "cand_" + digest(raw)
}

func ValidateCandidateRecord(c CandidateRecord) error {
	if c.ID == "" || c.WorkflowID == "" || c.TreeOID == "" || c.PolicyHash == "" || c.EngineVersion == "" {
		return ErrCandidateMismatch
	}
	manifest := append([]gitcandidate.ManifestEntry(nil), c.Manifest...)
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].Path < manifest[j].Path })
	if candidateRecordID(c.WorkflowID, c.TreeOID, c.PolicyHash, c.EngineVersion, c.Seal, manifest) != c.ID {
		return ErrCandidateMismatch
	}
	return nil
}

type EvidenceKind string

const (
	EvidenceAcceptance EvidenceKind = "acceptance"
	EvidenceCheck      EvidenceKind = "check"
	EvidenceReview     EvidenceKind = "review"
)

type Evidence struct {
	ID                string
	Kind              EvidenceKind
	CandidateRecordID string
	CandidateTreeOID  string
	PolicyHash        string
	Digest            string
	Lens              Lens
}

func ValidateEvidence(candidate CandidateRecord, evidence []Evidence) error {
	for _, item := range evidence {
		if item.ID == "" || item.Digest == "" || item.CandidateRecordID != candidate.ID || item.CandidateTreeOID != candidate.TreeOID {
			return fmt.Errorf("%w: %s", ErrEvidenceMismatch, item.ID)
		}
		if item.PolicyHash != candidate.PolicyHash {
			return fmt.Errorf("%w: %s", ErrPolicyMismatch, item.ID)
		}
	}
	return nil
}

func digest(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
func dedupe(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
