package render

import (
	"strings"
	"testing"

	"github.com/zylar06/video-agent/internal/domain"
)

func testAsset(id string) domain.MediaAsset {
	return domain.MediaAsset{
		ID:          id,
		ProjectID:   "p",
		Path:        "/media/" + id + ".mp4",
		ContentHash: strings.Repeat("a", 64),
		DurationUS:  10_000_000,
		Width:       640,
		Height:      360,
		HasAudio:    true,
		FPS:         "30/1",
		Status:      "ready",
	}
}

func testTimeline() domain.TimelineRevision {
	t := domain.TimelineRevision{
		ID: "t", ProjectID: "p", Revision: 1, ParentRevision: 0,
		FPSNum: 30, FPSDen: 1, Width: 640, Height: 360,
		Items: []domain.ClipItem{
			{ID: "c1", AssetID: "a1", SourceInUS: 0, SourceOutUS: 1_000_000, DurationFrames: 30},
			{ID: "c2", AssetID: "a2", SourceInUS: 2_000_000, SourceOutUS: 4_000_000, DurationFrames: 60},
		},
	}
	t.Reflow()
	return t
}

func TestCompileCarriesEveryFieldTheRendererNeeds(t *testing.T) {
	assets := []domain.MediaAsset{testAsset("a1"), testAsset("a2")}
	plan, err := Compile(testTimeline(), assets)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TimelineID != "t" || plan.Revision != 1 || plan.Width != 640 || plan.Height != 360 || plan.FPSNum != 30 || plan.FPSDen != 1 {
		t.Fatalf("header not carried over: %+v", plan)
	}
	if len(plan.Inputs) != 2 {
		t.Fatalf("expected 2 inputs, got %d", len(plan.Inputs))
	}
	want := []InputRange{
		{ClipID: "c1", AssetID: "a1", Path: "/media/a1.mp4", SourceInUS: 0, SourceOutUS: 1_000_000, StartFrame: 0, DurationFrames: 30, HasAudio: true, ContentHash: strings.Repeat("a", 64)},
		{ClipID: "c2", AssetID: "a2", Path: "/media/a2.mp4", SourceInUS: 2_000_000, SourceOutUS: 4_000_000, StartFrame: 30, DurationFrames: 60, HasAudio: true, ContentHash: strings.Repeat("a", 64)},
	}
	for i := range want {
		if plan.Inputs[i] != want[i] {
			t.Fatalf("input %d = %+v, want %+v", i, plan.Inputs[i], want[i])
		}
	}
}

func TestCompileRejectsInvalidTimelines(t *testing.T) {
	assets := []domain.MediaAsset{testAsset("a1"), testAsset("a2")}
	cases := map[string]func(*domain.TimelineRevision, *[]domain.MediaAsset){
		"empty timeline id":   func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.ID = "" },
		"revision below one":  func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.Revision = 0 },
		"odd width":           func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.Width = 641 },
		"unknown asset":       func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.Items[0].AssetID = "ghost" },
		"foreign project":     func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.ProjectID = "other" },
		"source beyond asset": func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.Items[0].SourceOutUS = 99_000_000 },
		"zero frame duration": func(tl *domain.TimelineRevision, _ *[]domain.MediaAsset) { tl.Items[0].DurationFrames = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			timeline := testTimeline()
			mutate(&timeline, &assets)
			if _, err := Compile(timeline, assets); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

// Compile checks the timeline header and asset ownership; the "a plan must
// have inputs" rule belongs to Plan.Validate, which runs before any render.
func TestCompileAcceptsEmptyTimelineButPlanValidateDoesNot(t *testing.T) {
	timeline := testTimeline()
	timeline.Items = nil
	timeline.Reflow()
	plan, err := Compile(timeline, []domain.MediaAsset{testAsset("a1"), testAsset("a2")})
	if err != nil {
		t.Fatalf("compiling an empty timeline is not an error: %v", err)
	}
	if len(plan.Inputs) != 0 {
		t.Fatalf("expected no inputs, got %d", len(plan.Inputs))
	}
	if err := plan.Validate(); err == nil {
		t.Fatal("an input-less plan must never reach the renderer")
	}
}

func TestCompileRejectsDuplicateAssetRecords(t *testing.T) {
	assets := []domain.MediaAsset{testAsset("a1"), testAsset("a2"), testAsset("a1")}
	if _, err := Compile(testTimeline(), assets); err == nil {
		t.Fatal("two records for one asset id must be rejected")
	}
}

func validPlan() Plan {
	return Plan{
		TimelineID: "t", Revision: 1, Width: 640, Height: 360, FPSNum: 30, FPSDen: 1,
		Inputs: []InputRange{
			{ClipID: "c1", AssetID: "a1", Path: "/media/a1.mp4", SourceInUS: 0, SourceOutUS: 1_000_000, StartFrame: 0, DurationFrames: 30, ContentHash: strings.Repeat("a", 64)},
		},
	}
}

func TestPlanValidateAcceptsACompiledPlan(t *testing.T) {
	plan, err := Compile(testTimeline(), []domain.MediaAsset{testAsset("a1"), testAsset("a2")})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("a compiled plan must validate: %v", err)
	}
	// Compile's output must survive a JSON round trip through the job record.
	if plan.Frames() != 90 {
		t.Fatalf("frames = %d, want 90", plan.Frames())
	}
	if plan.DurationUS() != 3_000_000 {
		t.Fatalf("duration = %d, want 3000000", plan.DurationUS())
	}
}

func TestPlanValidateRejectsMalformedPlans(t *testing.T) {
	cases := map[string]func(*Plan){
		"missing timeline id": func(p *Plan) { p.TimelineID = "" },
		"revision below one":  func(p *Plan) { p.Revision = 0 },
		"odd height":          func(p *Plan) { p.Height = 361 },
		"fps below one":       func(p *Plan) { p.FPSNum = 0 },
		"no inputs":           func(p *Plan) { p.Inputs = nil },
		"empty asset id":      func(p *Plan) { p.Inputs[0].AssetID = "" },
		"empty path":          func(p *Plan) { p.Inputs[0].Path = "" },
		"short content hash":  func(p *Plan) { p.Inputs[0].ContentHash = "abc" },
		"negative source in":  func(p *Plan) { p.Inputs[0].SourceInUS = -1 },
		"inverted source":     func(p *Plan) { p.Inputs[0].SourceOutUS = p.Inputs[0].SourceInUS },
		"gap before first":    func(p *Plan) { p.Inputs[0].StartFrame = 5 },
		"zero frame duration": func(p *Plan) { p.Inputs[0].DurationFrames = 0 },
		"frames vs source":    func(p *Plan) { p.Inputs[0].DurationFrames = 31 },
		"source beyond a day": func(p *Plan) { p.Inputs[0].SourceOutUS = 86_400_000_001 },
		"width beyond 8k":     func(p *Plan) { p.Width = 7682 },
		"timeline too long":   func(p *Plan) { p.Inputs[0].SourceOutUS = 86_400_000_000; p.Inputs[0].DurationFrames = 2_592_000_000 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			plan := validPlan()
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

// Only 1x playback is supported, so the frame count must round-trip through the
// rational frame rate for both integer and fractional rates.
func TestPlanFrameAndDurationRoundTrip(t *testing.T) {
	cases := []struct {
		name           string
		num, den       int
		sourceUS       int64
		wantFrames     int
		wantDurationUS int64
	}{
		{"integer rate", 30, 1, 1_000_000, 30, 1_000_000},
		{"fractional rate", 30000, 1001, 1_000_000, 30, 1_001_000},
		{"pal rate", 25, 1, 2_000_000, 50, 2_000_000},
		{"half second", 30, 1, 500_000, 15, 500_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			timeline := domain.TimelineRevision{FPSNum: tc.num, FPSDen: tc.den}
			frames := timeline.Frames(tc.sourceUS)
			if frames != tc.wantFrames {
				t.Fatalf("Frames(%d) = %d, want %d", tc.sourceUS, frames, tc.wantFrames)
			}
			plan := Plan{
				TimelineID: "t", Revision: 1, Width: 640, Height: 360, FPSNum: tc.num, FPSDen: tc.den,
				Inputs: []InputRange{{
					ClipID: "c1", AssetID: "a1", Path: "/m.mp4",
					SourceInUS: 0, SourceOutUS: tc.sourceUS,
					StartFrame: 0, DurationFrames: frames, ContentHash: strings.Repeat("a", 64),
				}},
			}
			if err := plan.Validate(); err != nil {
				t.Fatalf("plan must validate: %v", err)
			}
			if plan.Frames() != tc.wantFrames {
				t.Fatalf("Frames() = %d, want %d", plan.Frames(), tc.wantFrames)
			}
			if got := plan.DurationUS(); got != tc.wantDurationUS {
				t.Fatalf("DurationUS() = %d, want %d", got, tc.wantDurationUS)
			}
		})
	}
}
