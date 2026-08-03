package models

// PackageDefinition defines an installable package from the catalog.
type PackageDefinition struct {
	ID               string   `json:"id"`
	DisplayName      string   `json:"displayName"`
	RepoURL          string   `json:"repoUrl"`
	Adapter          string   `json:"adapter"`
	SupportedTargets []string `json:"supportedTargets"`
	DefaultVersion   string   `json:"defaultVersion"`
	RequiresNeurox   bool     `json:"requiresNeurox"`
	InstallStrategy  string   `json:"installStrategy"`
}

// InstallRequest is the user's resolved install request.
type InstallRequest struct {
	Packages          []string
	Targets           []string
	Versions          map[string]string
	Interactive       bool
	StateDir          string
	CleanupDeprecated bool
	// TrustSetupScripts is an explicit opt-in for package lifecycle scripts.
	TrustSetupScripts bool
	// SkillsDecisions is ephemeral and is not persisted in configuration.
	SkillsDecisions     map[string]string
	SkillsBundleVersion string
	SkillsBundleCommit  string
}

// ResolvedVersion holds version resolution results.
type ResolvedVersion struct {
	RequestedSelector string
	ResolvedVersion   string
	ResolvedRef       string
	Commit            string
	RepoURL           string
	Dirty             bool
}

// TargetResult is the result of installing for one target.
type TargetResult struct {
	Status      string   `json:"status"`
	InstalledAt string   `json:"installedAt"`
	Artifacts   []string `json:"artifacts"`
}

// InstallResult is the result of installing a package.
type InstallResult struct {
	PackageID        string
	RequestedVersion string
	ResolvedVersion  string
	ResolvedRef      string
	Commit           string
	Dirty            bool
	Targets          map[string]*TargetResult
}

// ValidationIssue is a preflight validation problem.
type ValidationIssue struct {
	Level     string // "error" or "warning"
	PackageID string
	Target    string
	Message   string
	FixHint   string
}

// Catalog is the package catalog structure.
type Catalog struct {
	Version  int                           `json:"version"`
	Packages map[string]*PackageDefinition `json:"packages"`
}
