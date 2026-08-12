//go:build !unix

package container

import (
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}

func killProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}

func signalName(_ *os.ProcessState) string { return "" }

func fileLinkCount(_ os.FileInfo) uint64 { return 0 }
