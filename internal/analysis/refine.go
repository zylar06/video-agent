package analysis

import (
	"context"
	"github.com/zylar06/video-agent/internal/analysis/visual"
	"github.com/zylar06/video-agent/internal/domain"
	"path/filepath"
	"strconv"
)

// Refine samples the actual boundaries and midpoint of each candidate.
// Local edits reuse these observations; expansion beyond the handles is rejected.
func (s *Service) Refine(ctx context.Context, p domain.DraftPlan) (domain.DraftPlan, error) {
	if s.Vision == nil {
		return p, nil
	}
	asset, err := s.Store.Asset(p.ProjectID, p.AssetID)
	if err != nil {
		return p, err
	}
	times := []int64{}
	for _, c := range p.Candidates {
		for _, t := range []int64{c.StartUS - 1_000_000, c.StartUS, c.StartUS + 1_000_000, (c.StartUS + c.EndUS) / 2, c.EndUS - 1_000_000, c.EndUS} {
			times = append(times, t)
		}
	}
	frames, err := visual.SampleTimes(ctx, s.Tools, asset, filepath.Join(s.Store.Dir, "analysis", asset.ID, "candidate-frames"), times)
	if err != nil {
		return p, err
	}
	key := "candidate-v1"
	existing, err := s.Store.Evidence(p.ProjectID, []string{p.AssetID})
	if err != nil {
		return p, err
	}
	cached := map[string]domain.Evidence{}
	for _, e := range existing {
		cached[e.ID] = e
	}
	for start := 0; start < len(frames); start += 8 {
		batch := frames[start:min(start+8, len(frames))]
		missing := []domain.Evidence{}
		for _, e := range batch {
			if cached[e.ID+"-"+key].VisualSummary == "" {
				missing = append(missing, e)
			}
		}
		if len(missing) > 0 {
			summaries, err := s.Vision.Describe(ctx, missing)
			if err != nil {
				return p, err
			}
			for _, e := range missing {
				e.VisualSummary = summaries[e.ID]
				e.ID += "-" + key
				e.CacheKey = key
				if _, err = s.Store.PutEvidence(e); err != nil {
					return p, err
				}
				cached[e.ID] = e
			}
		}
	}
	for i := range p.Candidates {
		c := &p.Candidates[i]
		count := 0
		for _, e := range frames {
			if e.StartUS >= c.MinStartUS && e.StartUS <= c.MaxEndUS {
				c.EvidenceIDs = append(c.EvidenceIDs, e.ID+"-"+key)
				count++
			}
		}
		c.Reasons = append(c.Reasons, "入出点及片段内部已补充 "+strconv.Itoa(count)+" 张实际画面证据；优先保留完整语句")
	}
	base := p.Version
	p.Version++
	return s.Store.UpdateDraft(p, base)
}
