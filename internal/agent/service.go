package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zylar06/video-agent/internal/analysis"
	"github.com/zylar06/video-agent/internal/app"
	"github.com/zylar06/video-agent/internal/catalog"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/edit"
	"github.com/zylar06/video-agent/internal/planner"
	"github.com/zylar06/video-agent/internal/store"
)

const APIVersion = "v1"

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Envelope struct {
	APIVersion string    `json:"api_version"`
	OK         bool      `json:"ok"`
	Result     any       `json:"result,omitempty"`
	Error      *APIError `json:"error,omitempty"`
}

// Service is the constrained P3 boundary used by both the JSON CLI and HTTP.
// It deliberately exposes typed actions rather than raw database or filesystem
// access, so an external Code Agent cannot bypass revisions or asset ownership.
type Service struct{ App *app.App }

func NewService(a *app.App) *Service { return &Service{App: a} }

func (s *Service) Names() []string {
	return []string{"analyze", "assets_import", "assets_list", "draft_confirm", "draft_create", "draft_edit", "draft_get", "edit_apply", "evidence_add", "jobs_cancel", "jobs_get", "jobs_list", "project_create", "project_get", "project_list", "proposal_create", "render_submit", "search", "timeline_create", "timeline_get", "timeline_history"}
}

func (s *Service) Call(ctx context.Context, name string, raw json.RawMessage) Envelope {
	result, err := s.call(ctx, name, raw)
	if err != nil {
		return Envelope{APIVersion: APIVersion, OK: false, Error: classify(err)}
	}
	return Envelope{APIVersion: APIVersion, OK: true, Result: result}
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("invalid input: expected exactly one JSON object")
	}
	return nil
}

func (s *Service) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "project_create":
		var in domain.Project
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return in, s.App.Store.CreateProject(in)
	case "project_list":
		var in struct{}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		projects, err := s.App.Store.Projects()
		if err != nil {
			return nil, err
		}
		// Include the asset count so the agent does not have to walk every
		// project with assets_list just to find the one holding footage.
		type projectSummary struct {
			domain.Project
			AssetCount int `json:"asset_count"`
		}
		out := make([]projectSummary, 0, len(projects))
		for _, p := range projects {
			assets, err := s.App.Store.Assets(p.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, projectSummary{Project: p, AssetCount: len(assets)})
		}
		return out, nil
	case "project_get":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.Project(in.ID)
	case "assets_import":
		var in struct {
			ProjectID string `json:"project_id"`
			Path      string `json:"path"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Tools.Import(ctx, s.App.Store, in.ProjectID, in.Path)
	case "assets_list":
		var in struct {
			ProjectID string `json:"project_id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if _, err := s.App.Store.Project(in.ProjectID); err != nil {
			return nil, err
		}
		return s.App.Store.Assets(in.ProjectID)
	case "draft_create":
		var in struct {
			ProjectID  string `json:"project_id"`
			AssetID    string `json:"asset_id"`
			Query      string `json:"query"`
			DurationUS int64  `json:"duration_us"`
			Limit      int    `json:"limit,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return planner.Service{Store: s.App.Store}.CreateFromQuery("draft-"+app.ID(), in.ProjectID, in.AssetID, in.Query, in.DurationUS, in.Limit)
	case "draft_get":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.Draft(in.ID)
	case "draft_edit":
		var in struct {
			ID          string `json:"id"`
			BaseVersion int    `json:"base_version"`
			CandidateID string `json:"candidate_id"`
			Kind        string `json:"kind"`
			StartUS     int64  `json:"start_us,omitempty"`
			EndUS       int64  `json:"end_us,omitempty"`
			ExtendStart int64  `json:"extend_start_us,omitempty"`
			ExtendEnd   int64  `json:"extend_end_us,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return planner.Service{Store: s.App.Store}.Apply(in.ID, in.BaseVersion, planner.Edit{
			CandidateID:   in.CandidateID,
			Kind:          in.Kind,
			StartUS:       in.StartUS,
			EndUS:         in.EndUS,
			ExtendStartUS: in.ExtendStart,
			ExtendEndUS:   in.ExtendEnd,
		})
	case "draft_confirm":
		var in struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.confirmDraft(in.ID, in.Version)
	case "timeline_create":
		var in domain.TimelineRevision
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.CreateTimeline(in)
	case "timeline_get":
		var in struct {
			ID       string `json:"id"`
			Revision int    `json:"revision,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		if in.Revision == 0 {
			return s.App.Store.Current(in.ID)
		}
		return s.App.Store.Revision(in.ID, in.Revision)
	case "timeline_history":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.History(in.ID)
	case "evidence_add":
		var in domain.Evidence
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.PutEvidence(in)
	case "analyze":
		var in struct {
			ProjectID       string         `json:"project_id"`
			AssetID         string         `json:"asset_id"`
			SubtitlePath    string         `json:"subtitle_path,omitempty"`
			AnalyzerVersion string         `json:"analyzer_version,omitempty"`
			Provider        string         `json:"provider,omitempty"`
			Parameters      map[string]any `json:"parameters,omitempty"`
			Visual          bool           `json:"visual,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return analysis.New(s.App.Store, s.App.Tools).Analyze(ctx, analysis.Request{ProjectID: in.ProjectID, AssetID: in.AssetID, SubtitlePath: in.SubtitlePath, AnalyzerVersion: in.AnalyzerVersion, Provider: in.Provider, Parameters: in.Parameters, Visual: in.Visual})
	case "search":
		var in catalog.SearchRequest
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		result, err := s.App.Store.SearchEvidence(in.ProjectID, in.Query, in.AssetIDs, in.Limit)
		if err != nil {
			return nil, err
		}
		if len(result) == 0 {
			return nil, errNoMatch(in.Query)
		}
		return result, nil
	case "proposal_create":
		var in struct {
			TimelineID string `json:"timeline_id"`
			Query      string `json:"query"`
			Limit      int    `json:"limit,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.propose(in.TimelineID, in.Query, in.Limit)
	case "edit_apply":
		var in domain.EditOperation
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return edit.NewEngine(s.App.Store).Apply(ctx, in)
	case "render_submit":
		var in struct {
			TimelineID string `json:"timeline_id"`
			Revision   int    `json:"revision,omitempty"`
			Preview    bool   `json:"preview,omitempty"`
			Filename   string `json:"filename,omitempty"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.SubmitRender(in.TimelineID, in.Revision, in.Preview, in.Filename)
	case "jobs_get":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.Job(in.ID)
	case "jobs_list":
		var in struct{}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.Store.Jobs()
	case "jobs_cancel":
		var in struct {
			ID string `json:"id"`
		}
		if err := decode(raw, &in); err != nil {
			return nil, err
		}
		return s.App.CancelJob(in.ID)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// confirmDraft is the only step where a reviewed draft becomes real: it
// materializes the candidates into a new timeline and freezes the draft in one
// transaction. Confirming twice is rejected rather than silently re-applied.
func (s *Service) confirmDraft(id string, version int) (domain.TimelineRevision, error) {
	draft, err := s.App.Store.Draft(id)
	if err != nil {
		return domain.TimelineRevision{}, err
	}
	if draft.Status != "draft" {
		return domain.TimelineRevision{}, errors.New("草稿已经确认过了，请在新的时间线上继续编辑")
	}
	if draft.Version != version {
		return domain.TimelineRevision{}, fmt.Errorf("%w: 草稿当前 version=%d，请求 version=%d", store.ErrConflict, draft.Version, version)
	}
	asset, err := s.App.Store.Asset(draft.ProjectID, draft.AssetID)
	if err != nil {
		return domain.TimelineRevision{}, err
	}
	timeline, err := planner.Timeline(draft, asset)
	if err != nil {
		return domain.TimelineRevision{}, err
	}
	if err = s.App.Store.ConfirmDraft(draft, timeline); err != nil {
		return domain.TimelineRevision{}, err
	}
	return timeline, nil
}

func (s *Service) propose(timelineID, query string, limit int) (domain.EditProposal, error) {
	if limit == 0 {
		limit = 3
	}
	t, err := s.App.Store.Current(timelineID)
	if err != nil {
		return domain.EditProposal{}, err
	}
	results, err := s.App.Store.SearchEvidence(t.ProjectID, query, nil, limit)
	if err != nil {
		return domain.EditProposal{}, err
	}
	if len(results) == 0 {
		return domain.EditProposal{}, errNoMatch(query)
	}
	p := domain.EditProposal{ID: "proposal-" + app.ID(), TimelineID: t.ID, BaseRevision: t.Revision, Query: query}
	for i, result := range results {
		e := result.Evidence
		p.EvidenceIDs = append(p.EvidenceIDs, e.ID)
		p.Operations = append(p.Operations, domain.EditOperation{ID: fmt.Sprintf("%s-%02d", p.ID, i+1), TimelineID: t.ID, BaseRevision: t.Revision + i, Kind: domain.OpInsertClip, NewClipID: fmt.Sprintf("candidate-%s-%02d", p.ID[len("proposal-"):], i+1), AssetID: e.AssetID, SourceInUS: e.StartUS, SourceOutUS: e.EndUS, Index: len(t.Items) + i})
	}
	return p, nil
}

type noMatchError struct{ query string }

func (e noMatchError) Error() string { return "no source evidence matches query: " + e.query }
func errNoMatch(query string) error  { return noMatchError{query: query} }

func classify(err error) *APIError {
	code := "invalid_request"
	switch {
	case errors.As(err, new(noMatchError)):
		code = "no_match"
	case errors.Is(err, store.ErrNotFound):
		code = "not_found"
	case errors.Is(err, store.ErrConflict):
		code = "revision_conflict"
	case errors.Is(err, context.Canceled):
		code = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case strings.Contains(err.Error(), "model"):
		code = "model_unavailable"
	case strings.Contains(err.Error(), "state conflict"):
		code = "revision_conflict"
	}
	return &APIError{Code: code, Message: err.Error()}
}
