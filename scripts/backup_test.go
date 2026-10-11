//go:build linux

package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBackupPublishesPrivateArchiveWithManifestAndRestarts(t *testing.T) {
	fixture := newBackupFixture(t, "success")
	cmd := fixture.command()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("backup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "created ") {
		t.Fatalf("backup did not report its published archive: %s", output)
	}

	dirInfo, err := os.Stat(fixture.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0700 {
		t.Fatalf("backup directory mode=%04o, want 0700", got)
	}
	archives, err := filepath.Glob(filepath.Join(fixture.backupDir, "voxmail-*.tar.gz"))
	if err != nil || len(archives) != 1 {
		t.Fatalf("published archives=%v err=%v, want one archive", archives, err)
	}
	archive := archives[0]
	archiveInfo, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if got := archiveInfo.Mode().Perm(); got != 0600 {
		t.Fatalf("archive mode=%04o, want 0600", got)
	}
	owner := archiveInfo.Sys().(*syscall.Stat_t)
	if owner.Uid != uint32(os.Getuid()) || owner.Gid != uint32(os.Getgid()) {
		t.Fatalf("archive owner=%d:%d, want invoking user %d:%d", owner.Uid, owner.Gid, os.Getuid(), os.Getgid())
	}

	name := filepath.Base(archive)
	checksumPath := archive + ".sha256"
	manifestPath := archive + ".manifest"
	for _, sidecar := range []string{checksumPath, manifestPath} {
		info, err := os.Stat(sidecar)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("sidecar %s mode=%04o, want 0600", filepath.Base(sidecar), got)
		}
	}
	checksum, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(mustReadFile(t, archive))
	wantChecksum := hex.EncodeToString(digest[:]) + "  " + name + "\n"
	if string(checksum) != wantChecksum {
		t.Fatalf("checksum=%q, want %q", checksum, wantChecksum)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"format=voxmail-backup-v1", "volume=test-volume", "archive=" + name, "sha256=" + hex.EncodeToString(digest[:])} {
		if !strings.Contains(string(manifest), field) {
			t.Errorf("manifest missing %q: %s", field, manifest)
		}
	}
	listing, err := exec.Command("tar", "-tzf", archive).CombinedOutput()
	if err != nil || !strings.Contains(string(listing), "./sqlite/voxmail.db") {
		t.Fatalf("archive does not contain database: err=%v listing=%s", err, listing)
	}
	if strings.Contains(string(mustReadFile(t, fixture.logPath)), "compose stop voxmail") == false ||
		!strings.Contains(string(mustReadFile(t, fixture.logPath)), "compose start voxmail") {
		t.Fatalf("running service was not stopped and restarted: %s", mustReadFile(t, fixture.logPath))
	}
	entries, err := os.ReadDir(fixture.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("backup directory has staging leftovers: %v", entryNames(entries))
	}
}

func TestInterruptedBackupRestartsServiceAndRemovesPartialArchive(t *testing.T) {
	fixture := newBackupFixture(t, "interrupt")
	cmd := fixture.command()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	waitBackupFile(t, fixture.readyPath)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 143 {
		t.Fatalf("interrupted backup exit=%v, want status 143", err)
	}
	entries, err := os.ReadDir(fixture.backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("interrupted backup left visible or staged output: %v", entryNames(entries))
	}
	log := string(mustReadFile(t, fixture.logPath))
	if !strings.Contains(log, "compose stop voxmail") || !strings.Contains(log, "compose start voxmail") {
		t.Fatalf("service was not restarted after interruption: %s", log)
	}
}

func TestRestoreDrillRejectsBadBackupChecksumBeforeDocker(t *testing.T) {
	root := t.TempDir()
	archiveName := "voxmail-20261007T120000Z-unique.tar.gz"
	archivePath := filepath.Join(root, archiveName)
	if err := os.WriteFile(archivePath, []byte("changed archive"), 0600); err != nil {
		t.Fatal(err)
	}
	wrongDigest := strings.Repeat("0", 64)
	if err := os.WriteFile(archivePath+".sha256", []byte(wrongDigest+"  "+archiveName+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := "format=voxmail-backup-v1\ncreated_at=20261007T120000Z\nvolume=test-volume\narchive=" + archiveName + "\nsha256=" + wrongDigest + "\n"
	if err := os.WriteFile(archivePath+".manifest", []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	dockerLog := filepath.Join(root, "docker-called")
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte("#!/bin/sh\nprintf called > \"$VOXMAIL_RESTORE_DOCKER_LOG\"\nexit 99\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "restore-drill.sh", archivePath, "image@sha256:abcdef")
	cmd.Dir = "."
	cmd.Env = envWithOverrides(map[string]string{
		"PATH":                       binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"VOXMAIL_ENCRYPTION_KEY":     "restore-test-key-with-at-least-32-bytes",
		"VOXMAIL_RESTORE_DOCKER_LOG": dockerLog,
	})
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("restore drill accepted corrupt archive checksum: %s", output)
	}
	if _, err := os.Stat(dockerLog); !os.IsNotExist(err) {
		t.Fatalf("restore drill called Docker before rejecting checksum: %v", err)
	}
}

type backupFixture struct {
	root      string
	backupDir string
	volumeDir string
	logPath   string
	readyPath string
	binDir    string
	mode      string
}

func newBackupFixture(t *testing.T, mode string) backupFixture {
	t.Helper()
	root := t.TempDir()
	backupDir := filepath.Join(root, "backups")
	volumeDir := filepath.Join(root, "volume")
	binDir := filepath.Join(root, "bin")
	for _, dir := range []string{backupDir, filepath.Join(volumeDir, "sqlite"), filepath.Join(volumeDir, "mail"), filepath.Join(volumeDir, "voices"), filepath.Join(volumeDir, "prompts"), binDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(volumeDir, "sqlite", "voxmail.db"), []byte("fixture database"), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "docker.log")
	readyPath := filepath.Join(root, "docker-ready")
	fakeDocker := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$VOXMAIL_FAKE_DOCKER_LOG"
if [ "$1" = volume ] && [ "$2" = inspect ]; then exit 0; fi
if [ "$1" = compose ]; then
  case "$2" in
    ps) printf '%s\n' voxmail ;;
    stop|start) exit 0 ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [ "$1" = run ]; then
  shift
  backup_dir=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --mount)
        mount=$2
        case "$mount" in
          *target=/backup)
            backup_dir=${mount#*source=}
            backup_dir=${backup_dir%,target=/backup}
            ;;
        esac
        shift 2
        ;;
      "$VOXMAIL_BACKUP_TOOL_IMAGE") shift; break ;;
      *) shift ;;
    esac
  done
  shift 5
  archive_name=$1
  if [ "$VOXMAIL_FAKE_DOCKER_MODE" = interrupt ]; then
    printf partial > "$backup_dir/$archive_name"
    : > "$VOXMAIL_FAKE_DOCKER_READY"
    exec sleep 60
  fi
  tar czf "$backup_dir/$archive_name" -C "$VOXMAIL_FAKE_VOLUME_DIR" .
  chmod 600 "$backup_dir/$archive_name"
  chown "$2:$3" "$backup_dir/$archive_name"
  exit 0
fi
exit 2
`
	if err := os.WriteFile(filepath.Join(binDir, "docker"), []byte(fakeDocker), 0700); err != nil {
		t.Fatal(err)
	}
	return backupFixture{root: root, backupDir: backupDir, volumeDir: volumeDir, logPath: logPath, readyPath: readyPath, binDir: binDir, mode: mode}
}

func (f backupFixture) command() *exec.Cmd {
	cmd := exec.Command("/bin/sh", "backup.sh")
	cmd.Dir = "."
	cmd.Env = envWithOverrides(map[string]string{
		"PATH":                      f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"VOXMAIL_BACKUP_DIR":        f.backupDir,
		"VOXMAIL_VOLUME_NAME":       "test-volume",
		"VOXMAIL_BACKUP_TOOL_IMAGE": "fake:latest",
		"VOXMAIL_FAKE_DOCKER_LOG":   f.logPath,
		"VOXMAIL_FAKE_DOCKER_READY": f.readyPath,
		"VOXMAIL_FAKE_VOLUME_DIR":   f.volumeDir,
		"VOXMAIL_FAKE_DOCKER_MODE":  f.mode,
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func envWithOverrides(overrides map[string]string) []string {
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[name]; !replaced {
			result = append(result, item)
		}
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}

func waitBackupFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for backup operation to begin")
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
