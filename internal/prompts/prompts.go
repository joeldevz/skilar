package prompts

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/joeldevz/skynex/internal/models"
	"github.com/joeldevz/skynex/internal/skillsync"
)

// BackupCapacityChoice is the action selected when an install has reached the
// retained snapshot cap.
type BackupCapacityChoice string

const (
	BackupRemoveOldest BackupCapacityChoice = "remove"
	BackupManage       BackupCapacityChoice = "manage"
	BackupCancel       BackupCapacityChoice = "cancel"
)

func ChooseBackupCapacity(input io.Reader, output io.Writer, summary string, canPrune bool) BackupCapacityChoice {
	choice := BackupCapacityChoice(BackupCancel)
	options := []huh.Option[string]{
		huh.NewOption("Manage backups", string(BackupManage)),
		huh.NewOption("Cancel", string(BackupCancel)),
	}
	if canPrune {
		options = append([]huh.Option[string]{huh.NewOption("Remove oldest backup and continue", string(BackupRemoveOldest)).Selected(true)}, options...)
	}
	field := huh.NewSelect[string]().Title(summary).Options(options...).Value((*string)(&choice))
	form := huh.NewForm(huh.NewGroup(field)).WithAccessible(accessibleMode()).WithTheme(wizardTheme(noColor())).WithInput(input).WithOutput(output)
	if err := form.Run(); err != nil {
		return BackupCancel
	}
	return choice
}

func ConfirmBackupPrune(input io.Reader, output io.Writer, count int) bool {
	confirmed := false
	form := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(fmt.Sprintf("Remove %d eligible backup(s)?", count)).Affirmative("Remove").Negative("Cancel").Value(&confirmed))).WithAccessible(accessibleMode()).WithTheme(wizardTheme(noColor())).WithInput(input).WithOutput(output)
	return form.Run() == nil && confirmed
}

// ResolveSkillDecisions presents only actionable modified entries. Unknown
// entries are deliberately informational and can never be selected.
func ResolveSkillDecisions(report skillsync.Report) (map[string]skillsync.Decision, error) {
	return ResolveSkillDecisionsWithIO(report, os.Stdin, os.Stdout)
}

func ResolveSkillDecisionsWithWriter(report skillsync.Report, out io.Writer) (map[string]skillsync.Decision, error) {
	return resolveSkillDecisionsWithIO(report, os.Stdin, out)
}

func ResolveSkillDecisionsWithIO(report skillsync.Report, input io.Reader, out io.Writer) (map[string]skillsync.Decision, error) {
	return resolveSkillDecisionsWithIO(report, input, out)
}

func resolveSkillDecisionsWithIO(report skillsync.Report, input io.Reader, out io.Writer) (map[string]skillsync.Decision, error) {
	if input == nil || out == nil {
		return nil, fmt.Errorf("prompt input and output are required")
	}
	decisions := make(map[string]skillsync.Decision)
	modified, retired, unknown := 0, 0, 0
	for _, entry := range report.Entries {
		switch entry.Status {
		case skillsync.Modified:
			modified++
		case skillsync.Retired:
			retired++
		case skillsync.Unknown:
			unknown++
		}
	}
	fmt.Fprintf(out, "\nSkills: %d modified, %d retired, %d unknown (unknown entries are preserved)\n", modified, retired, unknown)
	for _, entry := range report.Entries {
		if entry.Status != skillsync.Modified && entry.Status != skillsync.Retired {
			continue
		}
		choice := "keep"
		options := []huh.Option[string]{huh.NewOption("Keep local", "keep"), huh.NewOption("Replace packaged", "replace")}
		if entry.Status == skillsync.Retired {
			options = []huh.Option[string]{huh.NewOption("Keep local", "keep"), huh.NewOption("Retire packaged file", "replace")}
		}
		field := huh.NewSelect[string]().Title(fmt.Sprintf("%s [%s] local=%s bundle=%s", entry.Path, entry.Status, entry.LocalSHA256, entry.BundleSHA256)).Options(options...).Value(&choice)
		form := huh.NewForm(huh.NewGroup(field)).WithAccessible(accessibleMode()).WithTheme(wizardTheme(noColor())).WithInput(input).WithOutput(out)
		if err := form.Run(); err != nil {
			return nil, err
		}
		decisions[entry.Path] = skillsync.BindDecision(entry.Path, skillsync.Decision(choice), entry.LocalSHA256, entry.BundleSHA256, entry.BundleTreeSHA256)
	}
	return decisions, nil
}

// ConfirmPlan shows the install plan and asks for confirmation using Huh.
func ConfirmPlan(req *models.InstallRequest, _ *models.Catalog) bool {
	return ConfirmPlanWithIO(req, os.Stdin, os.Stdout)
}

func ConfirmPlanWithWriter(req *models.InstallRequest, input io.Reader, out io.Writer) bool {
	return ConfirmPlanWithIO(req, input, out)
}

func ConfirmPlanWithIO(req *models.InstallRequest, input io.Reader, out io.Writer) bool {
	if input == nil || out == nil {
		return false
	}
	// Keep rendering and interaction on the injected streams.
	if req == nil {
		return false
	}
	var plan strings.Builder
	for _, pkgID := range req.Packages {
		fmt.Fprintf(&plan, "  %s -> %s -> %s\n", pkgID, req.Versions[pkgID], strings.Join(req.Targets, ", "))
	}
	if noColor() {
		fmt.Fprintln(out, "\nInstall plan:\n"+plan.String())
	} else {
		fmt.Fprintln(out, lipgloss.NewStyle().Bold(true).Render("\nInstall plan:\n"+plan.String()))
	}
	confirmed := false
	form := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Proceed with installation?").Affirmative("Proceed").Negative("Cancel").Value(&confirmed))).WithAccessible(accessibleMode()).WithTheme(wizardTheme(noColor())).WithInput(input).WithOutput(out)
	return form.Run() == nil && confirmed
}

// ConfirmTrustSetupScripts makes the lifecycle-script opt-in an explicit
// interactive decision, rather than treating the command-line option alone as
// consent in a terminal session.
func ConfirmTrustSetupScripts() bool {
	return ConfirmTrustSetupScriptsWithIO(os.Stdin, os.Stdout)
}

func ConfirmTrustSetupScriptsWithIO(input io.Reader, output io.Writer) bool {
	if input == nil || output == nil {
		return false
	}
	confirmed := false
	confirm := huh.NewConfirm().Title("Allow npm/bun package setup scripts? They can execute arbitrary code.").Affirmative("Allow scripts").Negative("Keep scripts disabled").Value(&confirmed)
	form := huh.NewForm(huh.NewGroup(confirm)).WithAccessible(accessibleMode()).WithTheme(wizardTheme(noColor())).WithInput(input).WithOutput(output)
	return form.Run() == nil && confirmed
}
