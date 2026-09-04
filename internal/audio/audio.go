package audio

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type FFmpeg struct{ Binary string }

func (f FFmpeg) Convert(ctx context.Context, input, output string, sampleRate int) error {
	if f.Binary == "" {
		f.Binary = "ffmpeg"
	}
	if input == "" || output == "" || sampleRate < 8000 {
		return fmt.Errorf("invalid media conversion request")
	}
	command := exec.CommandContext(ctx, f.Binary, "-nostdin", "-y", "-i", input, "-ar", fmt.Sprint(sampleRate), "-ac", "1", "-f", "wav", output)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
