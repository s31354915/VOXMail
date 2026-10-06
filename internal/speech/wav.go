package speech

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const piperSynthesisTimeout = 2 * time.Minute

// validateWAV verifies the complete little-endian PCM WAV representation that
// Piper 1.3.0 writes. In particular, it checks the RIFF size and every chunk
// boundary against the actual file length; a file that has only received a
// header or a partial data chunk is rejected.
func validateWAV(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("audio output is not a regular file")
	}
	if info.Size() < 44 {
		return fmt.Errorf("audio output is too small to be a WAV")
	}
	if info.Size() > int64(^uint32(0))+8 {
		return fmt.Errorf("audio output exceeds RIFF size limit")
	}

	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("read WAV header: %w", err)
	}
	if !bytes.Equal(header[:4], []byte("RIFF")) || !bytes.Equal(header[8:12], []byte("WAVE")) {
		return fmt.Errorf("audio output is not RIFF/WAVE")
	}
	if int64(binary.LittleEndian.Uint32(header[4:8]))+8 != info.Size() {
		return fmt.Errorf("RIFF size does not match file length")
	}

	var (
		haveFmt       bool
		haveData      bool
		format        uint16
		channels      uint16
		sampleRate    uint32
		byteRate      uint32
		blockAlign    uint16
		bitsPerSample uint16
	)
	offset := int64(12)
	for offset < info.Size() {
		if info.Size()-offset < 8 {
			return fmt.Errorf("truncated WAV chunk header")
		}
		var chunkHeader [8]byte
		if _, err := file.ReadAt(chunkHeader[:], offset); err != nil {
			return fmt.Errorf("read WAV chunk header: %w", err)
		}
		chunkSize := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		paddedEnd := payloadEnd + chunkSize%2
		if payloadEnd < payloadStart || paddedEnd < payloadEnd || paddedEnd > info.Size() {
			return fmt.Errorf("WAV chunk exceeds file length")
		}
		switch string(chunkHeader[:4]) {
		case "fmt ":
			if haveFmt {
				return fmt.Errorf("duplicate WAV fmt chunk")
			}
			if chunkSize < 16 {
				return fmt.Errorf("WAV fmt chunk is truncated")
			}
			var formatHeader [16]byte
			if _, err := file.ReadAt(formatHeader[:], payloadStart); err != nil {
				return fmt.Errorf("read WAV fmt chunk: %w", err)
			}
			format = binary.LittleEndian.Uint16(formatHeader[0:2])
			channels = binary.LittleEndian.Uint16(formatHeader[2:4])
			sampleRate = binary.LittleEndian.Uint32(formatHeader[4:8])
			byteRate = binary.LittleEndian.Uint32(formatHeader[8:12])
			blockAlign = binary.LittleEndian.Uint16(formatHeader[12:14])
			bitsPerSample = binary.LittleEndian.Uint16(formatHeader[14:16])
			if format != 1 || channels == 0 || sampleRate == 0 || bitsPerSample != 16 {
				return fmt.Errorf("WAV is not 16-bit PCM")
			}
			if blockAlign != channels*2 || byteRate != sampleRate*uint32(blockAlign) {
				return fmt.Errorf("WAV format fields are inconsistent")
			}
			haveFmt = true
		case "data":
			if haveData {
				return fmt.Errorf("duplicate WAV data chunk")
			}
			if chunkSize == 0 {
				return fmt.Errorf("WAV data chunk is empty")
			}
			haveData = true
			if chunkSize%2 != 0 {
				return fmt.Errorf("WAV data is not aligned to a 16-bit sample")
			}
		}
		offset = paddedEnd
	}
	if offset != info.Size() {
		return fmt.Errorf("WAV has trailing incomplete data")
	}
	if !haveFmt || !haveData {
		return fmt.Errorf("WAV is missing fmt or data chunk")
	}
	return nil
}

// ValidWAV is the non-error form used by cache and startup discovery paths.
func ValidWAV(path string) bool { return validateWAV(path) == nil }

// synthesizePiper runs the pinned Piper CLI for exactly one input line. The
// v1.3.0 CLI has no JSON-input mode, so process completion is the authoritative
// completion signal. The temporary path is in the destination directory so
// rename is atomic on the same filesystem.
func synthesizePiper(ctx context.Context, p Piper, text, output string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if text == "" {
		return fmt.Errorf("cannot synthesize empty text")
	}
	if p.Binary == "" {
		p.Binary = "piper"
	}
	if p.Model == "" {
		return fmt.Errorf("piper model is required")
	}
	if err := safeOutput(output); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".piper-output-*.wav")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	defer os.Remove(tmpPath)

	args := []string{"--model", p.Model, "--output_file", tmpPath}
	args = append(args, p.Extra...)
	runCtx, cancel := context.WithTimeout(ctx, piperSynthesisTimeout)
	defer cancel()
	command := exec.CommandContext(runCtx, p.Binary, args...)
	configurePiperCommand(command)
	command.Stdin = strings.NewReader(text + "\n")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if runCtx.Err() != nil {
			return fmt.Errorf("piper synthesis timed out: %w", runCtx.Err())
		}
		return fmt.Errorf("piper: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := validateWAV(tmpPath); err != nil {
		return fmt.Errorf("piper produced invalid WAV: %w", err)
	}
	if err := os.Rename(tmpPath, output); err != nil {
		return fmt.Errorf("publish synthesized WAV: %w", err)
	}
	return nil
}
