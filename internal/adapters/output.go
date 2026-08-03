package adapters

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/x/term"
)

// Reporter is the install pipeline's only output boundary. Details are
// intentionally discarded in concise mode so they cannot corrupt a spinner.
type Reporter interface {
	Detail(format string, args ...interface{})
	Warning(format string, args ...interface{})
	Complete(checks []string)
}

type installReporter struct {
	out     io.Writer
	verbose bool
	color   bool
}

func NewReporter(out io.Writer, verbose bool) Reporter {
	color := false
	if file, ok := out.(*os.File); ok && os.Getenv("NO_COLOR") == "" {
		color = term.IsTerminal(file.Fd())
	}
	return &installReporter{out: out, verbose: verbose, color: color}
}

func (r *installReporter) Detail(format string, args ...interface{}) {
	if r.verbose {
		fmt.Fprintln(r.out, sanitizeTerminal(fmt.Sprintf(format, args...)))
	}
}

func (r *installReporter) Warning(format string, args ...interface{}) {
	if r.verbose {
		fmt.Fprintln(r.out, "Warning: "+sanitizeTerminal(fmt.Sprintf(format, args...)))
	}
}

// sanitizeTerminal removes C0/C1 controls and ANSI CSI/OSC sequences from
// output received from external tools. Newlines, carriage returns and tabs are
// retained so ordinary command output remains readable.
func sanitizeTerminal(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b == 0x1b {
			i++
			if i >= len(s) {
				break
			}
			if s[i] == '[' {
				for i++; i < len(s); i++ {
					if s[i] >= 0x40 && s[i] <= 0x7e {
						break
					}
				}
			} else if s[i] == ']' {
				for i++; i < len(s); i++ {
					if s[i] == 0x07 {
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i++
						break
					}
				}
			} else if s[i] < 0x20 {
				continue
			}
			continue
		}
		if b < 0x20 && b != '\n' && b != '\t' {
			continue
		}
		if b >= 0x80 && b <= 0x9f {
			continue
		}
		out = append(out, b)
	}
	return string(out)
}

type sanitizedWriter struct{ w io.Writer }

func (w sanitizedWriter) Write(p []byte) (int, error) {
	_, err := io.WriteString(w.w, sanitizeTerminal(string(p)))
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *installReporter) Complete(checks []string) {
	for _, check := range checks {
		line := "✓ " + sanitizeTerminal(check)
		if r.color {
			line = "\x1b[32m" + line + "\x1b[0m"
		}
		fmt.Fprintln(r.out, line)
	}
	cta := "You're ready for launch."
	if r.color {
		cta = "\x1b[36m" + cta + "\x1b[0m"
	}
	fmt.Fprintf(r.out, "\n%s\n\nRun:\n  opencode\n", cta)
}

func discardReporter() Reporter { return NewReporter(io.Discard, false) }
