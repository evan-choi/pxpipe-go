//go:build windows

package runner

import (
	"os"
	"os/exec"
)

func forwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func childExitCode(exitError *exec.ExitError) int {
	return exitError.ExitCode()
}
