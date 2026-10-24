//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

func configureWorkflowSupervisor(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
