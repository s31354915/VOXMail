package calls

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuietMeasurementAndCacheInvalidation(t *testing.T) {
	dir := t.TempDir()
	input, executable, count := filepath.Join(dir, "prompt.wav"), filepath.Join(dir, "measure"), filepath.Join(dir, "count")
	if err := os.WriteFile(input, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho measure >> '" + count + "'\necho '{\"input_i\":\"-38\",\"input_tp\":\"-25\"}' >&2\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := &PromptPlayer{}
	for i := 0; i < 2; i++ {
		filter := p.playbackFilter(context.Background(), executable, input)
		if !strings.Contains(filter, "volume=22dB") || !strings.Contains(filter, "latency=1") {
			t.Fatalf("quiet clip was not normalized with a flushed limiter: %q", filter)
		}
	}
	data, _ := os.ReadFile(count)
	if strings.Count(string(data), "measure") != 1 {
		t.Fatal("cached prompt was measured again")
	}
	if err := os.WriteFile(input, []byte("replacement fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	p.playbackFilter(context.Background(), executable, input)
	data, _ = os.ReadFile(count)
	if strings.Count(string(data), "measure") != 2 {
		t.Fatal("replaced audio reused its old gain")
	}
}

func TestTelephoneLoudnessMatchesAcrossLevelsAndDurations(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required for measured audio verification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	p := &PromptPlayer{Binary: ffmpeg}
	var levels []float64
	for i, tc := range []struct {
		seconds int
		volume  string
	}{{1, "0.05"}, {6, "2"}, {1, "0"}} {
		input, output := filepath.Join(dir, fmt.Sprintf("input%d.wav", i)), filepath.Join(dir, fmt.Sprintf("out%d.pcm", i))
		cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=22050:duration=%d", tc.seconds), "-af", "volume="+tc.volume, input)
		if log, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create audio: %v: %s", err, log)
		}
		if err := os.WriteFile(output, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := p.PlayMedia(ctx, output, input); err != nil {
			t.Fatal(err)
		}
		pcm, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if len(pcm) != tc.seconds*8000*2 {
			t.Fatalf("clip %d has %d samples; limiter lost or added audio", i, len(pcm)/2)
		}
		var squares, peak float64
		for j := 0; j+1 < len(pcm); j += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[j:j+2]))) / 32768
			squares += v * v
			peak = math.Max(peak, math.Abs(v))
		}
		if tc.volume == "0" {
			if peak != 0 {
				t.Fatal("normalization amplified silence")
			}
			continue
		}
		if peak > 0.842 {
			t.Fatalf("clip %d exceeded the peak ceiling: %f", i, peak)
		}
		levels = append(levels, 10*math.Log10(squares/float64(len(pcm)/2)))
	}
	if math.Abs(levels[0]-levels[1]) > 0.6 {
		t.Fatalf("quiet greeting and long prompt differ in loudness: %v dB", levels)
	}
	t.Logf("quiet short clip and loud long clip normalized to %.2f and %.2f dB RMS", levels[0], levels[1])
}
