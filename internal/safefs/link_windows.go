//go:build windows

package safefs

import "os"

// FileInfo does not expose Windows link counts portably; existing entries are
// rejected rather than treated as safe when the count cannot be verified.
func singleLink(os.FileInfo) bool { return false }

func SingleLink(info os.FileInfo) bool { return singleLink(info) }
