//go:build !unix

package mcpproxy

import "os"

func verifyProtectedFileIdentity(os.FileInfo) error { return nil }
