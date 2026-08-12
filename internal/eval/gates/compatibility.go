package gates

import (
	"strings"

	"github.com/joeldevz/skynex/internal/eval/baseline"
)

func EvaluateCompatibility(report baseline.CompatibilityReport) Result {
	if report.Compatible {
		return Result{Name: "compatibility", Status: StatusPass, Reason: ReasonGateSatisfied}
	}
	detail := strings.Join(report.Errors, "; ")
	if detail == "" {
		detail = "fingerprints are incompatible"
	}
	return invalidResult("compatibility", ReasonCompatibility, detail)
}
