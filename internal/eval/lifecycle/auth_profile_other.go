//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package lifecycle

import "os"

func validateCredentialFileOwner(os.FileInfo) error { return nil }
