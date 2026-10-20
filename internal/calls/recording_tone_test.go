package calls

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestRecordingToneWAVHeaderValid parses the generated start tone as a strict
// RIFF/WAVE file. The header once read "WAVEfmt " into the four-byte slot at
// offset 8, truncating the "fmt " chunk id and leaving bytes 12-15 zeroed;
// ffmpeg then rejected every tone with "no 'fmt ' tag found". This test walks
// the chunk chain so a malformed header cannot land again.
func TestRecordingToneWAVHeaderValid(t *testing.T) {
	wav := recordingToneWAV()
	if len(wav) < 44 {
		t.Fatalf("tone is %d bytes, too short for a WAV header", len(wav))
	}
	if got := string(wav[0:4]); got != "RIFF" {
		t.Fatalf("chunk id = %q, want RIFF", got)
	}
	riffSize := binary.LittleEndian.Uint32(wav[4:8])
	if int(riffSize)+8 != len(wav) {
		t.Fatalf("RIFF size %d + 8 != len %d", riffSize, len(wav))
	}
	if got := string(wav[8:12]); got != "WAVE" {
		t.Fatalf("riff type = %q, want WAVE", got)
	}
	if got := string(wav[12:16]); got != "fmt " {
		t.Fatalf("fmt chunk id = %q, want \"fmt \" (got %x)", got, wav[12:16])
	}
	fmtSize := binary.LittleEndian.Uint32(wav[16:20])
	if fmtSize != 16 {
		t.Fatalf("fmt chunk size = %d, want 16", fmtSize)
	}
	format := binary.LittleEndian.Uint16(wav[20:22])
	channels := binary.LittleEndian.Uint16(wav[22:24])
	rate := binary.LittleEndian.Uint32(wav[24:28])
	byteRate := binary.LittleEndian.Uint32(wav[28:32])
	blockAlign := binary.LittleEndian.Uint16(wav[32:34])
	bits := binary.LittleEndian.Uint16(wav[34:36])
	if format != 1 {
		t.Fatalf("audio format = %d, want 1 (PCM)", format)
	}
	if channels != 1 {
		t.Fatalf("channels = %d, want 1", channels)
	}
	if rate != 8000 {
		t.Fatalf("sample rate = %d, want 8000", rate)
	}
	if byteRate != rate*uint32(channels)*uint32(bits)/8 {
		t.Fatalf("byte rate = %d, want %d", byteRate, rate*uint32(channels)*uint32(bits)/8)
	}
	if blockAlign != channels*bits/8 {
		t.Fatalf("block align = %d, want %d", blockAlign, channels*bits/8)
	}
	if got := string(wav[36:40]); got != "data" {
		t.Fatalf("data chunk id = %q, want data", got)
	}
	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if int(dataSize)+44 != len(wav) {
		t.Fatalf("data size %d + 44 != len %d", dataSize, len(wav))
	}
	// The chunk chain must terminate exactly at the end of the buffer.
	if bytes.Count(wav[:44], []byte("data")) != 1 {
		t.Fatalf("unexpected duplicate data marker in header")
	}
	if dataSize == 0 {
		t.Fatalf("tone carries no samples")
	}
}
