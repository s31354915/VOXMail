// Exercise production speech, playback, sync-runner and SIP diagnostic paths
// against the real image binaries using only local disposable fixtures.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/voxmail/voxmail/internal/calls"
	"github.com/voxmail/voxmail/internal/diagnostics"
	"github.com/voxmail/voxmail/internal/mailsync"
	"github.com/voxmail/voxmail/internal/sip"
	"github.com/voxmail/voxmail/internal/speech"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("", "verbose-acceptance-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, diagnostics.HandlerOptions())))
	wav := filepath.Join(root, "speech.wav")
	piper := speech.Piper{Model: os.Getenv("VOXMAIL_PIPER_MODEL")}
	if err := piper.Synthesize(ctx, "The quick brown fox jumps over the lazy dog.", wav); err != nil {
		return err
	}
	recognitionWAV := filepath.Join(root, "recognition.wav")
	if err := diagnostics.Run(ctx, "ffmpeg", exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-i", wav, "-ar", "16000", "-ac", "1", recognitionWAV)); err != nil {
		return err
	}
	whisper := speech.Whisper{Model: os.Getenv("VOXMAIL_WHISPER_MODEL")}
	if text, err := whisper.Transcribe(ctx, recognitionWAV); err != nil || strings.TrimSpace(text) == "" {
		return fmt.Errorf("transcription failed: %v", err)
	}
	fifo := filepath.Join(root, "playback.pcm")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		return err
	}
	pcm := make(chan int64, 1)
	go func() {
		file, err := os.Open(fifo)
		if err != nil {
			pcm <- 0
			return
		}
		defer file.Close()
		size, _ := io.Copy(io.Discard, file)
		pcm <- size
	}()
	player := calls.PromptPlayer{}
	if err := player.PlayMedia(ctx, fifo, wav); err != nil {
		return err
	}
	select {
	case count := <-pcm:
		if count == 0 {
			return fmt.Errorf("empty PCM")
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, dir := range []string{"far/Inbox/new", "far/Inbox/cur", "far/Inbox/tmp", "near"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "far/Inbox/new/1"), []byte("From: fixture@example.test\nSubject: fixture\n\nLocal mail."), 0600); err != nil {
		return err
	}
	conf := fmt.Sprintf("MaildirStore far\nPath %s/far/\nInbox %s/far/Inbox\n\nMaildirStore near\nPath %s/near/\nInbox %s/near/Inbox\n\nChannel fixture\nMaster :far:\nSlave :near:\nPatterns INBOX\nCreate Both\nSyncState *\n", root, root, root, root)
	config := filepath.Join(root, "mbsyncrc")
	if err := os.WriteFile(config, []byte(conf), 0600); err != nil {
		return err
	}
	if _, err := (mailsync.Runner{}).Sync(ctx, config, "fixture"); err != nil {
		return err
	}
	if err := os.Setenv("VOXMAIL_SIP_ACCOUNT", "<sip:fixture@127.0.0.1:15061>;auth_user=fixture;auth_pass=fixture-password;regint=0"); err != nil {
		return err
	}
	if err := os.Setenv("VOXMAIL_SIP_LOCAL_PORT", "15060"); err != nil {
		return err
	}
	baresip := &sip.Baresip{ConfigDir: filepath.Join(root, "sip"), ControlSocket: filepath.Join(root, "sip.sock"), AudioDir: filepath.Join(root, "audio"), LogPath: filepath.Join(root, "baresip.log")}
	baresip.SetLifetime(ctx)
	if err := baresip.Apply(ctx); err != nil {
		return err
	}
	defer baresip.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(baresip.LogPath)
		if strings.Contains(string(data), "voxmail") && baresip.Status().Process == sip.ProcessRunning {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("native bridge did not start: %s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	baresip.Stop()
	data, err := os.ReadFile(baresip.LogPath)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("fixture-password")) {
		return fmt.Errorf("SIP credential leaked")
	}
	if diagnostics.Enabled() {
		if !bytes.Contains(data, []byte("verbose bridge diagnostics enabled")) {
			return fmt.Errorf("native verbose trace missing: %s", data)
		}
		for _, component := range []string{"piper", "whisper", "ffmpeg", "mbsync"} {
			if !strings.Contains(logs.String(), `"component":"`+component+`"`) {
				return fmt.Errorf("missing component trace %s", component)
			}
		}
		for _, output := range []string{"Loading voice", "whisper_init", "Input #", "Opening"} {
			if !strings.Contains(logs.String(), output) {
				return fmt.Errorf("missing engine diagnostics %q: %s", output, logs.String())
			}
		}
	} else if logs.Len() != 0 || bytes.Contains(data, []byte("verbose bridge diagnostics enabled")) {
		return fmt.Errorf("verbose diagnostics present in normal mode: %s", logs.String())
	}
	fmt.Printf("verbose acceptance passed: enabled=%v\n", diagnostics.Enabled())
	return nil
}
