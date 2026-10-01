package render

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zylar06/video-agent/internal/media"
)

func filterExpressions(t *testing.T, p Plan) []string {
	t.Helper()
	args := Arguments(p, "/tmp/out.mp4")
	for i, a := range args {
		if a == "-filter_complex" {
			if i+1 >= len(args) {
				t.Fatal("-filter_complex has no value")
			}
			return strings.Split(args[i+1], ";")
		}
	}
	t.Fatal("no -filter_complex in arguments")
	return nil
}

func argsContain(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func twoInputPlan(secondHasAudio bool) Plan {
	return Plan{
		TimelineID: "t", Revision: 1, Width: 640, Height: 360, FPSNum: 30, FPSDen: 1,
		Inputs: []InputRange{
			{ClipID: "c1", AssetID: "a1", Path: "/media/a1.mp4", SourceInUS: 1_000_000, SourceOutUS: 2_000_000, StartFrame: 0, DurationFrames: 30, HasAudio: true, ContentHash: strings.Repeat("a", 64)},
			{ClipID: "c2", AssetID: "a2", Path: "/media/a2.mp4", SourceInUS: 0, SourceOutUS: 2_000_000, StartFrame: 30, DurationFrames: 60, HasAudio: secondHasAudio, ContentHash: strings.Repeat("b", 64)},
		},
	}
}

// Every segment needs its own normalisation chain, and the concatenation must
// join exactly those chains.
func TestArgumentsNormalizesEachInputThenConcatenates(t *testing.T) {
	plan := twoInputPlan(true)
	args := Arguments(plan, "/tmp/out.mp4")
	filters := filterExpressions(t, plan)

	// Each input contributes a video and an audio chain; one concat joins them.
	if len(filters) != 2*len(plan.Inputs)+1 {
		t.Fatalf("expected a video+audio chain per input plus concat, got %d: %v", len(filters), filters)
	}
	for n := range plan.Inputs {
		video := filters[n*2]
		audio := filters[n*2+1]
		if !strings.HasPrefix(video, "["+itoa(n)+":v:0]") || !strings.HasSuffix(video, "[v"+itoa(n)+"]") {
			t.Fatalf("video filter %d malformed: %s", n, video)
		}
		for _, stage := range []string{"setpts=PTS-STARTPTS", "scale=640:360", "pad=640:360", "setsar=1", "fps=30/1", "format=yuv420p"} {
			if !strings.Contains(video, stage) {
				t.Fatalf("video filter %d missing %s: %s", n, stage, video)
			}
		}
		if !strings.HasSuffix(audio, "[a"+itoa(n)+"]") {
			t.Fatalf("audio filter %d malformed: %s", n, audio)
		}
	}
	concat := filters[len(filters)-1]
	if concat != "[v0][a0][v1][a1]concat=n=2:v=1:a=1[v][a]" {
		t.Fatalf("concat filter malformed: %s", concat)
	}
	if !argsContain(args, "[v]") || !argsContain(args, "[a]") {
		t.Fatalf("the concatenated streams must be mapped: %v", args)
	}
}

// A silent source must be given generated silence, never a reference to an
// audio stream that does not exist.
func TestArgumentsSuppressAudioForSilentInput(t *testing.T) {
	plan := twoInputPlan(false)
	filters := filterExpressions(t, plan)
	second := filters[3]
	if !strings.HasPrefix(second, "anullsrc=r=48000:cl=stereo") {
		t.Fatalf("silent input must use anullsrc: %s", second)
	}
	if strings.Contains(second, "[1:a:0]") {
		t.Fatalf("silent input must not reference a missing audio stream: %s", second)
	}
	if !strings.Contains(filters[1], "[0:a:0]") {
		t.Fatalf("the audio-bearing input must still read its own stream: %s", filters[1])
	}
}

// Frame-rate conversion at a range boundary is handled by padding the tail and
// then trimming to the exact requested frame count.
func TestArgumentsTrimToExactFrameCount(t *testing.T) {
	plan := twoInputPlan(true)
	filters := filterExpressions(t, plan)
	if !strings.Contains(filters[0], "trim=end_frame=30") {
		t.Fatalf("first segment must trim to 30 frames: %s", filters[0])
	}
	if !strings.Contains(filters[2], "trim=end_frame=60") {
		t.Fatalf("second segment must trim to 60 frames: %s", filters[2])
	}
	if !strings.Contains(filters[0], "tpad=stop_mode=clone") {
		t.Fatalf("short input must be padded before trimming: %s", filters[0])
	}
}

// Audio is trimmed by sample count derived from the frame count, so a segment
// cannot drift out of sync with its video.
func TestArgumentsDeriveAudioSampleCount(t *testing.T) {
	cases := []struct {
		num, den int
		frames   int
		want     int64
	}{
		{30, 1, 30, 48000},
		{30, 1, 60, 96000},
		{25, 1, 25, 48000},
		{30000, 1001, 30, 48048},
	}
	for _, tc := range cases {
		plan := Plan{
			TimelineID: "t", Revision: 1, Width: 640, Height: 360, FPSNum: tc.num, FPSDen: tc.den,
			Inputs: []InputRange{{
				ClipID: "c1", AssetID: "a1", Path: "/m.mp4", SourceInUS: 0,
				SourceOutUS: int64(tc.frames) * 1_000_000 * int64(tc.den) / int64(tc.num),
				StartFrame:  0, DurationFrames: tc.frames, HasAudio: true, ContentHash: strings.Repeat("a", 64),
			}},
		}
		audio := filterExpressions(t, plan)[1]
		want := "atrim=end_sample=" + itoa64(tc.want)
		if !strings.Contains(audio, want) {
			t.Fatalf("%d/%d %d frames: audio filter %s, want %s", tc.num, tc.den, tc.frames, audio, want)
		}
	}
}

func TestArgumentsTargetAndEncoding(t *testing.T) {
	plan := twoInputPlan(true)
	target := filepath.Join(t.TempDir(), "out.mp4")
	args := Arguments(plan, target)
	if args[len(args)-1] != target {
		t.Fatalf("target must be the final argument: %v", args)
	}
	for _, want := range []string{"libx264", "aac", "yuv420p", "48000", "+faststart"} {
		if !argsContain(args, want) {
			t.Fatalf("missing %q in arguments: %v", want, args)
		}
	}
	if !argsContain(args, "-xerror") || !argsContain(args, "explode") {
		t.Fatalf("strict error detection must be on: %v", args)
	}
	// Source in/out must be bound per input, not applied globally.
	if !argsContain(args, "1.000000") || !argsContain(args, "0.000000") {
		t.Fatalf("source offsets missing: %v", args)
	}
}

func TestExecuteRejectsBadTargetsBeforeRunningFFmpeg(t *testing.T) {
	// A nonexistent binary proves no process is started for these paths.
	tools := media.Tools{FFmpeg: "/nonexistent-ffmpeg", FFprobe: "/nonexistent-ffprobe"}
	dir := t.TempDir()

	plan := twoInputPlan(true)
	if _, err := Execute(context.Background(), tools, plan, filepath.Join(dir, "out.mkv")); err == nil {
		t.Fatal("a non-mp4 target must be rejected")
	}

	existing := filepath.Join(dir, "already.mp4")
	if err := os.WriteFile(existing, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), tools, plan, existing); err == nil {
		t.Fatal("an existing target must never be overwritten")
	}

	invalid := plan
	invalid.Inputs = nil
	if _, err := Execute(context.Background(), tools, invalid, filepath.Join(dir, "invalid.mp4")); err == nil {
		t.Fatal("an invalid plan must be rejected before rendering")
	}
	if _, err := os.Stat(filepath.Join(dir, "invalid.mp4")); !os.IsNotExist(err) {
		t.Fatal("a rejected render must not create its target")
	}
}

func itoa(n int) string     { return strconv.Itoa(n) }
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
