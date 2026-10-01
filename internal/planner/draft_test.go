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
