package chat

import (
	"testing"

	"github.com/zylar06/video-agent/internal/domain"
)

func TestClassifySupportedRequests(t *testing.T) {
	tests := []struct {
		message, goal, query string
		duration             int64
	}{
		{"剪成 1 分钟高能集锦", "highlights", "高能", 60_000_000},
		{"找出所有笑点", "jokes", "笑", 0},
		{"保留进球和庆祝", "sports", "进球 庆祝", 0},
		{"删掉开场和片尾", "trim_ends", "", 0},
		{"生成预览", "preview", "", 0},
		{"保留产品发布的掌声片段，剪成 30 秒", "select", "产品发布的掌声", 30_000_000},
	}
	for _, tt := range tests {
		got := classify(tt.message)
		if got.Goal != tt.goal || got.Query != tt.query || got.DurationUS != tt.duration {
			t.Fatalf("%q: got %+v", tt.message, got)
		}
	}
}

// A duration-only request such as "一分钟以上" must still produce candidates,
// otherwise the whole proposal path dead-ends.
func TestRankEvidencePrefersConfirmedClipsThenLength(t *testing.T) {
	all := []domain.Evidence{
		{ID: "a", StartUS: 0, EndUS: 40_000_000},
		{ID: "b", StartUS: 0, EndUS: 5_000_000, VisualSummary: "画面为红色"},
		{ID: "c", StartUS: 0, EndUS: 60_000_000},
		{ID: "d", StartUS: 0, EndUS: 8_000_000, VisualSummary: "画面为蓝色"},
	}
	got := rankEvidence(all, 4)
	if len(got) != 4 {
		t.Fatalf("want 4 candidates, got %d", len(got))
	}
	// Clips with a visual summary come first, longest of those leading.
	want := []string{"d", "b", "c", "a"}
	for i, id := range want {
		if got[i].Evidence.ID != id {
			t.Fatalf("position %d: want %s, got %s", i, id, got[i].Evidence.ID)
		}
	}
}

func TestRankEvidenceHandlesEmptyAndLimit(t *testing.T) {
	if got := rankEvidence(nil, 12); got != nil {
		t.Fatalf("want nil for no evidence, got %d", len(got))
	}
	all := []domain.Evidence{
		{ID: "a", StartUS: 0, EndUS: 10_000_000},
		{ID: "b", StartUS: 0, EndUS: 20_000_000},
		{ID: "c", StartUS: 0, EndUS: 30_000_000},
	}
	if got := rankEvidence(all, 2); len(got) != 2 {
		t.Fatalf("want 2 after limit, got %d", len(got))
	}
	// The input slice must not be reordered in place.
	if all[0].ID != "a" || all[2].ID != "c" {
		t.Fatalf("rankEvidence mutated its input: %+v", all)
	}
}

func TestReplyForFallbackSelection(t *testing.T) {
	got := reply(Intent{Goal: "best", Query: "一分钟以上"}, 3)
	if got == "" || got == reply(Intent{Goal: "select"}, 0) {
		t.Fatalf("fallback reply should explain the substitution, got %q", got)
	}
}
