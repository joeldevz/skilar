package adapters

import (
	"fmt"
	"os"
	"os/exec"
)

// Test-only seam for replacing the validated path immediately before it is
// opened. Production code leaves this nil.
var beforeInstallCwdOpen func() error

func sameInstallCwdIdentity(expected, current os.FileInfo) bool {
	return expected != nil && current != nil &&
		os.SameFile(expected, current) &&
		expected.IsDir() && current.IsDir() &&
		expected.Mode().Type() == current.Mode().Type()
}

// runWithInstallCwd starts a dependency manager in a descriptor-backed
// directory. The platform implementation fails closed when it cannot provide
// that guarantee.
func runWithInstallCwd(cwd *installCwd, cmd *exec.Cmd) error {
	if err := cwd.configure(cmd); err != nil {
		return err
	}
	runErr := cmd.Run()
	cwd.keepAlive()
	verifyErr := cwd.verify(cwd.path)
	if runErr != nil {
		return runErr
	}
	if verifyErr != nil {
		return fmt.Errorf("revalidate dependency install directory after subprocess: %w", verifyErr)
	}
	return nil
}

func unsupportedInstallCwdError() error {
	return fmt.Errorf("dependency installation requires a descriptor-backed working directory; this platform does not provide a safe mechanism")
}
