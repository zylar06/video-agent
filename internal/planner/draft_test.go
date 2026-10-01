package planner

import (
	"testing"

	"github.com/zylar06/video-agent/internal/catalog"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/store"
)

func TestDraftUsesTranscriptBoundariesAndAppliesEdits(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateProject(domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	asset := domain.MediaAsset{ID: "a", ProjectID: "p", Path: "/a.mp4", ContentHash: "hash", DurationUS: 20_000_000, Width: 1280, Height: 720, FPS: "30/1", Status: "ready"}
	if _, err = s.PutAsset(asset); err != nil {
		t.Fatal(err)
	}
	for _, e := range []domain.Evidence{
		{ID: "cue-1", ProjectID: "p", AssetID: "a", AssetContentHash: "hash", StartUS: 2_000_000, EndUS: 4_000_000, Transcript: "第一句"},
		{ID: "cue-2", ProjectID: "p", AssetID: "a", AssetContentHash: "hash", StartUS: 4_000_000, EndUS: 7_000_000, Transcript: "关键结论"},
	} {
		if _, err = s.PutEvidence(e); err != nil {
			t.Fatal(err)
		}
	}
	match := catalog.SearchResult{Evidence: domain.Evidence{ID: "cue-2", ProjectID: "p", AssetID: "a", AssetContentHash: "hash", StartUS: 4_000_000, EndUS: 7_000_000, Transcript: "关键结论"}, Score: 1, Reason: "matched transcript"}
	p, err := (Service{Store: s}).Create("draft-1", "p", "a", "关键结论", 5_000_000, []catalog.SearchResult{match})
	if err != nil || len(p.Candidates) != 1 {
		t.Fatalf("draft: %+v, %v", p, err)
	}
	c := p.Candidates[0]
	if c.StartUS != 4_000_000 || c.EndUS != 7_000_000 {
		t.Fatalf("candidate did not keep whole sentences: %+v", c)
	}
	p, err = (Service{Store: s}).Apply(p.ID, p.Version, Edit{CandidateID: c.ID, Kind: "lock"})
	if err != nil || !p.Candidates[0].Locked || p.Version != 2 {
		t.Fatalf("lock: %+v, %v", p, err)
	}
	if _, err = (Service{Store: s}).Apply(p.ID, 1, Edit{CandidateID: c.ID, Kind: "delete"}); err != store.ErrConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
}

// setupDraft builds one project/asset/evidence and a draft with a single
// candidate, so boundary edits can be asserted exactly.
func setupDraft(t *testing.T, assetDurationUS int64, evStart, evEnd int64) (*store.Store, domain.DraftPlan) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.CreateProject(domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	asset := domain.MediaAsset{ID: "a", ProjectID: "p", Path: "/a.mp4", ContentHash: "hash", DurationUS: assetDurationUS, Width: 1280, Height: 720, FPS: "30/1", Status: "ready"}
	if _, err = s.PutAsset(asset); err != nil {
		t.Fatal(err)
	}
	evidence := domain.Evidence{ID: "cue-1", ProjectID: "p", AssetID: "a", AssetContentHash: "hash", StartUS: evStart, EndUS: evEnd, Transcript: "关键结论"}
	if _, err = s.PutEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	matches, err := s.SearchEvidence("p", "关键结论", []string{"a"}, 3)
	if err != nil || len(matches) != 1 {
		t.Fatalf("search: %+v, %v", matches, err)
	}
	p, err := (Service{Store: s}).CreateFromQuery("draft-1", "p", "a", "关键结论", 5_000_000, 3)
	if err != nil || len(p.Candidates) != 1 {
		t.Fatalf("draft: %+v, %v", p, err)
	}
	return s, p
}

// A caller asking for "a bit longer" must not have to compute absolute bounds,
// and must not be able to leave the verified safe range.
func TestDraftRelativeExtendStaysInsideSafeHandles(t *testing.T) {
	s, p := setupDraft(t, 20_000_000, 4_000_000, 7_000_000)
	svc := Service{Store: s}
	c := p.Candidates[0]
	if c.StartUS != 4_000_000 || c.EndUS != 7_000_000 {
		t.Fatalf("unexpected starting bounds: %+v", c)
	}
	if c.MinStartUS != 2_000_000 || c.MaxEndUS != 9_000_000 {
		t.Fatalf("unexpected safety handles: %+v", c)
	}

	// Exactly to the handle is allowed.
	longer, err := svc.Apply(p.ID, 1, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: 2_000_000})
	if err != nil {
		t.Fatalf("extend to the handle: %v", err)
	}
	if longer.Version != 2 {
		t.Fatalf("version = %d, want 2", longer.Version)
	}
	if got := longer.Candidates[0]; got.StartUS != 4_000_000 || got.EndUS != 9_000_000 {
		t.Fatalf("relative extend did not move only the end boundary: %+v", got)
	}

	// Bounds are frame-quantized, so a sub-frame nudge lands on the same frame
	// and is a no-op rather than an error. A real step past the handle is not.
	if noop, err := svc.Apply(p.ID, 2, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: 1}); err != nil {
		t.Fatalf("a sub-frame nudge should be a no-op: %v", err)
	} else if noop.Candidates[0].EndUS != 9_000_000 {
		t.Fatalf("a sub-frame nudge moved the boundary: %+v", noop.Candidates[0])
	}
	if _, err = svc.Apply(p.ID, 3, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: frameUS("30/1")}); err == nil {
		t.Fatal("extending past max_end_us must be rejected")
	}
	// And the 2s floor still holds in the other direction.
	if _, err = svc.Apply(p.ID, 2, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: -6_000_000}); err == nil {
		t.Fatal("shrinking below the 2s minimum must be rejected")
	}
	still, err := s.Draft(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The sub-frame nudge and the two rejections must leave version at 3.
	if still.Version != 3 {
		t.Fatalf("unexpected version %d after rejected edits", still.Version)
	}
	if got := still.Candidates[0]; got.StartUS != 4_000_000 || got.EndUS != 9_000_000 {
		t.Fatalf("a rejected edit changed the bounds: %+v", got)
	}

	// A start extension is independent of the end boundary.
	final, err := svc.Apply(p.ID, 3, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendStartUS: -1_000_000})
	if err != nil {
		t.Fatalf("extend start: %v", err)
	}
	if got := final.Candidates[0]; got.StartUS != 3_000_000 || got.EndUS != 9_000_000 {
		t.Fatalf("extend_start_us moved the wrong boundary: %+v", got)
	}
}

// A locked candidate is protected from boundary edits until it is unlocked.
func TestDraftLockedCandidateRejectsBoundaryEdits(t *testing.T) {
	s, p := setupDraft(t, 20_000_000, 4_000_000, 7_000_000)
	svc := Service{Store: s}
	c := p.Candidates[0]
	locked, err := svc.Apply(p.ID, 1, Edit{CandidateID: c.ID, Kind: "lock"})
	if err != nil || !locked.Candidates[0].Locked {
		t.Fatalf("lock: %+v, %v", locked, err)
	}
	if _, err = svc.Apply(p.ID, 2, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: 1_000_000}); err == nil {
		t.Fatal("a locked candidate must reject boundary edits")
	}
	unlocked, err := svc.Apply(p.ID, 2, Edit{CandidateID: c.ID, Kind: "unlock"})
	if err != nil || unlocked.Candidates[0].Locked {
		t.Fatalf("unlock: %+v, %v", unlocked, err)
	}
	if _, err = svc.Apply(p.ID, 3, Edit{CandidateID: c.ID, Kind: "adjust_bounds", ExtendEndUS: 1_000_000}); err != nil {
		t.Fatalf("an unlocked candidate must accept edits again: %v", err)
	}
}

// Absolute bounds keep working for callers that already know them.
func TestDraftAbsoluteBoundsStillApply(t *testing.T) {
	s, p := setupDraft(t, 20_000_000, 4_000_000, 7_000_000)
	edited, err := (Service{Store: s}).Apply(p.ID, 1, Edit{CandidateID: p.Candidates[0].ID, Kind: "adjust_bounds", StartUS: 3_000_000, EndUS: 8_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if got := edited.Candidates[0]; got.StartUS != 3_000_000 || got.EndUS != 8_000_000 {
		t.Fatalf("absolute bounds were not applied: %+v", got)
	}
}

func TestCreateFromQueryRejectsUnknownTopic(t *testing.T) {
	s, _ := setupDraft(t, 20_000_000, 4_000_000, 7_000_000)
	if _, err := (Service{Store: s}).CreateFromQuery("draft-2", "p", "a", "完全不存在的主题", 5_000_000, 3); err == nil {
		t.Fatal("a topic with no evidence must be reported, not silently drafted")
	}
}
