package codexcli

import (
	"os/exec"
	"syscall"
)

func configureCommand(cmd *exec.Cmd) {
	// Windows uses the native executable rather than a shell/npm launcher.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
