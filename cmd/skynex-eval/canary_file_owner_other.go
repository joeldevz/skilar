//go:build !unix

package main

import (
	"errors"
	"io/fs"
)

func fileInfoOwnedByRoot(fs.FileInfo) (bool, error) {
	return false, errors.New("root-owned Workflow V2 plugin verification is unsupported on this platform")
}
