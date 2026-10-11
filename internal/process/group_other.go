//go:build !linux

package process

import "os/exec"

func configure(cmd *exec.Cmd) { cmd.WaitDelay = PipeWait }
func terminate(cmd *exec.Cmd) {}
