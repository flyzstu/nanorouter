//go:build !windows

package agybridge

import (
	"errors"
	"os/exec"
	"syscall"
)

// configureProcess 让 agy 及其子进程处在独立进程组，取消请求时可以整组杀掉。
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
