//go:build windows

package agybridge

import "os/exec"

func configureProcess(_ *exec.Cmd) {}

func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
