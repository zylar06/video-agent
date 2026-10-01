package domain

import (
	"errors"
	"fmt"
	"time"
)

// CandidateSegment is an evidence-backed clip suggested before a timeline is
// created. Its bounds can be adjusted only inside the verified safe range.
type CandidateSegment struct {
	ID          string   `json:"id"`
	AssetID     string   `json:"asset_id"`
	StartUS     int64    `json:"start_us"`
	EndUS       int64    `json:"end_us"`
	MinStartUS  int64    `json:"min_start_us"`
	MaxEndUS    int64    `json:"max_end_us"`
	EvidenceIDs []string `json:"evidence_ids"`
	Score       float64  `json:"score"`
	Reasons     []string `json:"reasons"`
	Locked      bool     `json:"locked"`
	Deleted     bool     `json:"deleted"`
}

// DraftPlan is a revisioned, user-reviewable edit proposal. It is deliberately
// separate from TimelineRevision so confirmation remains an explicit action.
type DraftPlan struct {
	ID               string             `json:"id"`
	ProjectID        string             `json:"project_id"`
	AssetID          string             `json:"asset_id"`
	AnalysisCacheKey string             `json:"analysis_cache_key"`
	Query            string             `json:"query"`
	TargetDurationUS int64              `json:"target_duration_us"`
	Version          int                `json:"version"`
	Status           string             `json:"status"`
	Candidates       []CandidateSegment `json:"candidates"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
}

func (p DraftPlan) Validate(asset MediaAsset) error {
	if p.ID == "" || p.ProjectID == "" || p.AssetID == "" || p.ProjectID != asset.ProjectID || p.AssetID != asset.ID || p.TargetDurationUS <= 0 || p.Version < 1 || (p.Status != "draft" && p.Status != "confirmed") {
		return errors.New("invalid draft plan")
	}
	seen := map[string]bool{}
	active := 0
	for _, c := range p.Candidates {
		if c.ID == "" || seen[c.ID] || c.AssetID != asset.ID || c.StartUS < c.MinStartUS || c.EndUS > c.MaxEndUS || c.MinStartUS < 0 || c.MaxEndUS > asset.DurationUS || c.EndUS <= c.StartUS || len(c.EvidenceIDs) == 0 {
			return fmt.Errorf("invalid candidate %q", c.ID)
		}
		seen[c.ID] = true
		if !c.Deleted {
			active++
		}
	}
	if active == 0 {
		return errors.New("draft plan requires an active candidate")
	}
	return nil
}
