package reporter

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DefaultTerminalTextBytes = 16 << 10

// SanitizeTerminal removes ANSI/ECMA-48 escape sequences and makes remaining
// control/format characters visible. It also bounds output to prevent an
// untrusted trace field from flooding a terminal.
func SanitizeTerminal(value string) string {
	return SanitizeTerminalN(value, DefaultTerminalTextBytes)
}

func SanitizeTerminalN(value string, maximumBytes int) string {
	if maximumBytes <= 0 {
		return ""
	}
	withoutEscapes := stripEscapeSequences(value)
	var output strings.Builder
	truncated := false
	for len(withoutEscapes) > 0 {
		r, size := utf8.DecodeRuneInString(withoutEscapes)
		if r == utf8.RuneError && size == 1 {
			piece := "\\x" + fmt.Sprintf("%02x", withoutEscapes[0])
			if output.Len()+len(piece) > maximumBytes {
				truncated = true
				break
			}
			output.WriteString(piece)
			withoutEscapes = withoutEscapes[1:]
			continue
		}
		withoutEscapes = withoutEscapes[size:]
		piece := string(r)
		switch {
		case r == '\n' || r == '\t':
		case r == '\r':
			piece = "\\r"
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf):
			if r <= 0xff {
				piece = fmt.Sprintf("\\x%02x", r)
			} else if r <= 0xffff {
				piece = fmt.Sprintf("\\u%04x", r)
			} else {
				piece = fmt.Sprintf("\\U%08x", r)
			}
		}
		if output.Len()+len(piece) > maximumBytes {
			truncated = true
			break
		}
		output.WriteString(piece)
	}
	if truncated {
		marker := "…[truncated]"
		if output.Len()+len(marker) <= maximumBytes {
			output.WriteString(marker)
		}
	}
	return output.String()
}

func stripEscapeSequences(value string) string {
	var output strings.Builder
	for i := 0; i < len(value); {
		if value[i] != 0x1b {
			output.WriteByte(value[i])
			i++
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		switch value[i] {
		case '[': // CSI: terminate at a final byte in 0x40..0x7e.
			i++
			for i < len(value) {
				b := value[i]
				i++
				if b >= 0x40 && b <= 0x7e {
					break
				}
			}
		case ']': // OSC: BEL or ST terminates.
			i++
			i = consumeControlString(value, i, true)
		case 'P', 'X', '^', '_': // DCS, SOS, PM, APC: ST terminates.
			i++
			i = consumeControlString(value, i, false)
		default:
			// Two-byte escape sequence. Swallow its final byte.
			i++
		}
	}
	return output.String()
}

func consumeControlString(value string, index int, bellTerminates bool) int {
	for index < len(value) {
		if bellTerminates && value[index] == 0x07 {
			return index + 1
		}
		if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
			return index + 2
		}
		index++
	}
	return index
}
