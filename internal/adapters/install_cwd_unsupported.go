//go:build !linux && !windows

package adapters

import (
	"fmt"
	"os"
	"os/exec"
)

type installCwd struct {
	path string
}

func openInstallCwd(string, os.FileInfo) (*installCwd, error) {
	return nil, unsupportedInstallCwdError()
}

func (*installCwd) configure(*exec.Cmd) error { return unsupportedInstallCwdError() }
func (*installCwd) verify(string) error       { return unsupportedInstallCwdError() }
func (*installCwd) keepAlive()                {}
func (*installCwd) Close() error {
	return fmt.Errorf("dependency install directory handle is unavailable")
}
