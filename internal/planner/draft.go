// Package planner turns evidence matches into a reviewable draft plan.
package planner

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/zylar06/video-agent/internal/catalog"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/store"
)

const minCandidateUS int64 = 2_000_000

type Service struct{ Store *store.Store }

type Edit struct {
	CandidateID string `json:"candidate_id"`
	Kind        string `json:"kind"`
	StartUS     int64  `json:"start_us,omitempty"`
	EndUS       int64  `json:"end_us,omitempty"`
}

func (s Service) Create(id, projectID, assetID, query string, durationUS int64, matches []catalog.SearchResult) (domain.DraftPlan, error) {
	if durationUS <= 0 {
		return domain.DraftPlan{}, errors.New("请说明成片时长，例如“剪成 60 秒”")
	}
	asset, err := s.Store.Asset(projectID, assetID)
	if err != nil {
		return domain.DraftPlan{}, err
	}
	all, err := s.Store.CurrentEvidence(projectID, assetID)
	if err != nil {
		return domain.DraftPlan{}, err
	}
	transcript := make([]domain.Evidence, 0, len(all))
	for _, e := range all {
		if strings.TrimSpace(e.Transcript) != "" {
			transcript = append(transcript, e)
		}
	}
	sort.Slice(transcript, func(i, j int) bool { return transcript[i].StartUS < transcript[j].StartUS })
	plan := domain.DraftPlan{ID: id, ProjectID: projectID, AssetID: assetID, Query: query, TargetDurationUS: durationUS, Version: 1, Status: "draft"}
	if len(matches) > 0 {
		plan.AnalysisCacheKey = matches[0].Evidence.CacheKey
	}
	remaining := durationUS
	used := [][2]int64{}
	for _, match := range matches {
		candidate := makeCandidate(id, len(plan.Candidates)+1, asset, match, transcript)
		for _, pause := range all {
			if pause.Provider != "ffmpeg-silence" {
				continue
			}
			mid := (pause.StartUS + pause.EndUS) / 2
			if mid <= candidate.StartUS && candidate.StartUS-mid <= 400_000 {
				candidate.StartUS = snap(asset.FPS, mid)
			}
			if mid >= candidate.EndUS && mid-candidate.EndUS <= 400_000 {
				candidate.EndUS = min(asset.DurationUS, snap(asset.FPS, mid))
			}
		}
		if overlaps(candidate.StartUS, candidate.EndUS, used) {
			continue
		}
		if candidate.EndUS-candidate.StartUS > remaining {
			if len(plan.Candidates) > 0 && candidate.EndUS-candidate.StartUS-remaining > remaining {
				continue
			}
		}
		plan.Candidates = append(plan.Candidates, candidate)
		used = append(used, [2]int64{candidate.StartUS, candidate.EndUS})
		remaining -= candidate.EndUS - candidate.StartUS
		if remaining < minCandidateUS {
			break
		}
	}
	if len(plan.Candidates) == 0 {
		return domain.DraftPlan{}, errors.New("没有足够的候选片段满足指定时长")
	}
	sort.Slice(plan.Candidates, func(i, j int) bool { return plan.Candidates[i].StartUS < plan.Candidates[j].StartUS })
	return s.Store.CreateDraft(plan)
}

func makeCandidate(planID string, n int, asset domain.MediaAsset, match catalog.SearchResult, transcript []domain.Evidence) domain.CandidateSegment {
	e := match.Evidence
	start, end := e.StartUS, e.EndUS
	ids := []string{e.ID}
	for _, cue := range transcript {
		if cue.EndUS <= e.StartUS || cue.StartUS >= e.EndUS {
			continue
		}
		start = min(start, cue.StartUS)
		end = max(end, cue.EndUS)
		if cue.ID != e.ID {
			ids = append(ids, cue.ID)
		}
	}
	if end-start < minCandidateUS {
		end = min(asset.DurationUS, start+minCandidateUS)
		start = max(int64(0), end-minCandidateUS)
	}
	start, end = snap(asset.FPS, start), snap(asset.FPS, end)
	if end <= start {
		end = min(asset.DurationUS, start+frameUS(asset.FPS))
	}
	reasons := []string{"匹配“" + match.Reason + "”"}
	if e.VisualSummary != "" {
		reasons = append(reasons, "画面证据已参与判断")
	}
	return domain.CandidateSegment{ID: planID + "-candidate-" + strconv.Itoa(n), AssetID: asset.ID, StartUS: start, EndUS: end, MinStartUS: max(int64(0), start-2_000_000), MaxEndUS: min(asset.DurationUS, end+2_000_000), EvidenceIDs: ids, Score: match.Score, Reasons: reasons}
}

func (s Service) Apply(draftID string, baseVersion int, edit Edit) (domain.DraftPlan, error) {
	p, err := s.Store.Draft(draftID)
	if err != nil {
		return p, err
	}
	if p.Status != "draft" || p.Version != baseVersion {
		return p, store.ErrConflict
	}
	asset, err := s.Store.Asset(p.ProjectID, p.AssetID)
	if err != nil {
		return p, err
	}
	for i := range p.Candidates {
		c := &p.Candidates[i]
		if c.ID != edit.CandidateID {
			continue
		}
		if c.Locked && (edit.Kind == "adjust_bounds" || edit.Kind == "delete") {
			return p, errors.New("请先解除锁定再修改片段")
		}
		switch edit.Kind {
		case "adjust_bounds":
			start, end := snap(asset.FPS, edit.StartUS), snap(asset.FPS, edit.EndUS)
			if start < c.MinStartUS || end > c.MaxEndUS || end-start < minCandidateUS {
				return p, errors.New("调整后的片段必须位于安全范围内且至少保留 2 秒")
			}
			c.StartUS, c.EndUS = start, end
		case "delete":
			c.Deleted = true
		case "lock":
			c.Locked = true
		case "unlock":
			c.Locked = false
		default:
			return p, errors.New("unsupported draft edit")
		}
		p.Version++
		return s.Store.UpdateDraft(p, baseVersion)
	}
	return p, store.ErrNotFound
}

func Timeline(p domain.DraftPlan, asset domain.MediaAsset) (domain.TimelineRevision, error) {
	if p.Status != "draft" {
		return domain.TimelineRevision{}, errors.New("draft has already been confirmed")
	}
	num, den := fps(asset.FPS)
	t := domain.TimelineRevision{ID: "timeline-" + p.ID, ProjectID: p.ProjectID, Revision: 1, FPSNum: num, FPSDen: den, Width: asset.Width, Height: asset.Height}
	for _, c := range p.Candidates {
		if c.Deleted {
			continue
		}
		t.Items = append(t.Items, domain.ClipItem{ID: "clip-" + c.ID, AssetID: c.AssetID, SourceInUS: c.StartUS, SourceOutUS: c.EndUS, DurationFrames: t.Frames(c.EndUS - c.StartUS), Locked: c.Locked, EvidenceIDs: append([]string(nil), c.EvidenceIDs...)})
	}
	if len(t.Items) == 0 {
		return t, errors.New("draft has no active clips")
	}
	t.Reflow()
	return t, nil
}

func overlaps(start, end int64, used [][2]int64) bool {
	for _, r := range used {
		if start < r[1] && end > r[0] {
			return true
		}
	}
	return false
}

func fps(raw string) (int, int) {
	parts := strings.Split(raw, "/")
	if len(parts) == 2 {
		n, nerr := strconv.Atoi(parts[0])
		d, derr := strconv.Atoi(parts[1])
		if nerr == nil && derr == nil && n > 0 && d > 0 && n <= 120000 && n <= 120*d {
			return n, d
		}
	}
	return 30, 1
}

func frameUS(raw string) int64 {
	n, d := fps(raw)
	return int64(math.Round(1e6 * float64(d) / float64(n)))
}

func snap(raw string, us int64) int64 {
	n, d := fps(raw)
	frames := math.Round(float64(us) * float64(n) / (1e6 * float64(d)))
	return int64(math.Round(frames * 1e6 * float64(d) / float64(n)))
}
