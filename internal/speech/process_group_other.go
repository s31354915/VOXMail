//go:build !linux

package speech

import (
	"os/exec"
	"time"
)

// Platforms without the Linux process-group primitive still get a bounded
// exec.Cmd pipe wait; the direct child is canceled by CommandContext.
func configurePiperCommand(command *exec.Cmd) {
	command.WaitDelay = 500 * time.Millisecond
}
