//go:build windows

package workflow

import "os"

func fileHasMultipleLinks(os.FileInfo) bool { return false }
