//go:build !windows

package codexcli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureCommand(cmd *exec.Cmd) {
	// npm's Unix launcher spawns a native binary. Kill its process group on
	// cancellation so neither the launcher nor the actual CLI is left running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
