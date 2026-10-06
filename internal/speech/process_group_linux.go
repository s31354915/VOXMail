//go:build linux

package speech

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// configurePiperCommand makes cancellation terminate the CLI and any helper
// processes it created, then bounds exec.Cmd's pipe-drain wait. Go's default
// CommandContext cancellation kills only the direct process; Wait can otherwise
// remain blocked while an orphaned descendant still owns stderr.
func configurePiperCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 500 * time.Millisecond
}
