package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/store"
)

func TestLocalFileRejectsNonRegularAndMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := LocalFile(filepath.Join(dir, "nope.mp4")); err == nil {
		t.Fatal("a missing path must be rejected")
	}
	if _, err := LocalFile(dir); err == nil {
		t.Fatal("a directory must be rejected")
	}
	file := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := LocalFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("expected an absolute path, got %q", resolved)
	}
}

func TestHashIsContentAddressed(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.bin")
	second := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(first, []byte("same bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("same bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := Hash(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("identical content must hash equally: %s != %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected a hex sha256, got %q", a)
	}
	if err := os.WriteFile(second, []byte("same bytez"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Hash(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("a single changed byte must change the hash")
	}
}

// Hash must abort on a cancelled context rather than finishing a long read.
func TestHashHonorsCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Hash(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSecondsFormatting(t *testing.T) {
	cases := map[int64]string{0: "0.000000", 1_500_000: "1.500000", 90_000_000: "90.000000", 33_333: "0.033333"}
	for us, want := range cases {
		if got := Seconds(us); got != want {
			t.Fatalf("Seconds(%d) = %q, want %q", us, got, want)
		}
	}
}

func TestRateParsingRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{"", "30", "30/0", "a/b", "30/-1", "/"} {
		if got := Rate(raw); got != 0 {
			t.Fatalf("Rate(%q) = %v, want 0", raw, got)
		}
	}
	if got := Rate("30000/1001"); got < 29.9 || got > 30.0 {
		t.Fatalf("Rate(30000/1001) = %v", got)
	}
	if got := Rate("25/1"); got != 25 {
		t.Fatalf("Rate(25/1) = %v", got)
	}
}

func TestDefaultHonorsEnvOverrides(t *testing.T) {
	t.Setenv("VIDEO_AGENT_FFMPEG", "/custom/ffmpeg")
	t.Setenv("VIDEO_AGENT_FFPROBE", "/custom/ffprobe")
	tools := Default()
	if tools.FFmpeg != "/custom/ffmpeg" || tools.FFprobe != "/custom/ffprobe" {
		t.Fatalf("env overrides ignored: %+v", tools)
	}
}

// Arguments are passed as argv, so shell metacharacters must survive verbatim.
// If Run ever used a shell, the command would execute instead of echoing.
func TestRunUsesArgvNotShell(t *testing.T) {
	payload := "; touch pwned; $(echo sub)"
	out, err := Run(context.Background(), "/bin/sh", "-c", "printf '%s' \"$0\"", payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != payload {
		t.Fatalf("argument was mangled: %q", string(out))
	}
}

func TestRunReportsStderrOnFailure(t *testing.T) {
	_, err := Run(context.Background(), "/bin/sh", "-c", "echo diagnostic >&2; exit 3")
	if err == nil {
		t.Fatal("a non-zero exit must be an error")
	}
	if !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("stderr missing from error: %v", err)
	}
}

// A cancelled context must surface as a context error, not as the killed
// process's exit status.
func TestRunHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, "/bin/sh", "-c", "sleep 5"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestRunTruncatesLargeStdout(t *testing.T) {
	out, err := Run(context.Background(), "/bin/sh", "-c", "head -c 8388608 /dev/zero | tr '\\0' 'a'")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4<<20 {
		t.Fatalf("stdout was not clamped: %d bytes", len(out))
	}
	if len(out) == 0 {
		t.Fatal("expected output to be kept up to the cap")
	}
}

func TestRunReportsMissingExecutable(t *testing.T) {
	if _, err := Run(context.Background(), "/nonexistent-video-agent-exe"); err == nil {
		t.Fatal("a missing executable must be an error")
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateProject(domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImportRejectsUnknownProjectAndNonFile(t *testing.T) {
	s := testStore(t)
	tools := Tools{FFmpeg: "/nonexistent-ffmpeg", FFprobe: "/nonexistent-ffprobe"}
	if _, err := tools.Import(context.Background(), s, "missing-project", "/tmp/whatever.mp4"); err == nil {
		t.Fatal("an unknown project must be rejected before any file work")
	}
	if _, err := tools.Import(context.Background(), s, "p", filepath.Join(s.Dir, "nope.mp4")); err == nil {
		t.Fatal("a missing file must be rejected")
	}
	if _, err := tools.Import(context.Background(), s, "p", s.Dir); err == nil {
		t.Fatal("a directory must be rejected")
	}
}

// A probe failure must not leave a half-imported snapshot behind.
func TestImportRefusesUnprobeableInput(t *testing.T) {
	s := testStore(t)
	source := filepath.Join(t.TempDir(), "not-really-video.mp4")
	if err := os.WriteFile(source, []byte("this is not a video"), 0600); err != nil {
		t.Fatal(err)
	}
	tools := Tools{FFmpeg: "/nonexistent-ffmpeg", FFprobe: "/nonexistent-ffprobe"}
	if _, err := tools.Import(context.Background(), s, "p", source); err == nil {
		t.Fatal("an unprobeable input must fail the import")
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "assets"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".media") {
			t.Fatalf("failed import left a snapshot behind: %s", entry.Name())
		}
	}
}

func TestProbeRejectsMissingFile(t *testing.T) {
	tools := Tools{FFmpeg: "/nonexistent-ffmpeg", FFprobe: "/nonexistent-ffprobe"}
	if _, err := tools.Probe(context.Background(), filepath.Join(t.TempDir(), "gone.mp4")); err == nil {
		t.Fatal("probing a missing file must fail before invoking ffprobe")
	}
}

func TestLimitedBufferCapsAndStillReportsFull(t *testing.T) {
	b := &limitedBuffer{limit: 4}
	n, err := b.Write([]byte("abcdefgh"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("Write must report the full length so io.Copy keeps draining: %d", n)
	}
	if string(b.data) != "abcd" {
		t.Fatalf("buffer exceeded its cap: %q", b.data)
	}
	if _, err := b.Write([]byte("ij")); err != nil {
		t.Fatal(err)
	}
	if string(b.data) != "abcd" {
		t.Fatalf("a full buffer must stay at the cap: %q", b.data)
	}
}

// Probe derives its own deadline from the caller's context, so cancelling
// before the call must abort rather than run ffprobe to completion.
func TestProbeHonorsCancelledContext(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 60\n"), 0700); err != nil {
		t.Fatal(err)
	}
	tools := Tools{FFprobe: stub}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := tools.Probe(ctx, writeTempFile(t, "input.mp4", "x")); err == nil {
		t.Fatal("a cancelled probe must fail")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe ignored the caller's cancellation: %s", elapsed)
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
