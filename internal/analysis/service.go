package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zylar06/video-agent/internal/analysis/asr"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/analysis/visual"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/media"
	"github.com/zylar06/video-agent/internal/store"
)

type Request struct {
	ProjectID       string         `json:"project_id"`
	AssetID         string         `json:"asset_id"`
	SubtitlePath    string         `json:"subtitle_path,omitempty"`
	AnalyzerVersion string         `json:"analyzer_version"`
	Provider        string         `json:"provider"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	Visual          bool           `json:"visual,omitempty"`
}

type Result struct {
	Run      domain.AnalysisRun `json:"run"`
	Evidence []domain.Evidence  `json:"evidence"`
}

type ASRProvider interface {
	Transcribe(context.Context, domain.MediaAsset) ([]asr.Cue, error)
}
type VisionProvider interface {
	Describe(context.Context, []domain.Evidence) (map[string]string, error)
}

type Service struct {
	Store  *store.Store
	Tools  media.Tools
	ASR    ASRProvider
	Vision VisionProvider
}

func New(s *store.Store, tools media.Tools) *Service {
	asrConfig := provider.ConfigFromEnvAliases("VIDEO_AGENT_ASR", "AUTOCLIP_ASR")
	visionConfig := provider.ConfigFromEnvAliases("VIDEO_AGENT_VISION", "AUTOCLIP_VISION")
	var asrProvider ASRProvider
	if asrConfig.BaseURL != "" && asrConfig.Model != "" && asrConfig.APIKey != "" {
		if strings.HasPrefix(asrConfig.Model, "qwen-audio-") {
			asrProvider = provider.QwenAudioASR{Config: asrConfig, Tools: tools}
		} else if strings.HasPrefix(asrConfig.Model, "qwen") {
			asrProvider = provider.QwenASR{Config: asrConfig, Tools: tools}
		} else {
			asrProvider = provider.OpenAITranscriber{Config: asrConfig}
		}
	}
	var visionProvider VisionProvider
	if visionConfig.BaseURL != "" && visionConfig.Model != "" && visionConfig.APIKey != "" {
		visionProvider = provider.OpenAIVision{Config: visionConfig}
	}
	return &Service{Store: s, Tools: tools, ASR: asrProvider, Vision: visionProvider}
}

func NewWithProviders(s *store.Store, tools media.Tools, asrProvider ASRProvider, visionProvider VisionProvider) *Service {
	return &Service{Store: s, Tools: tools, ASR: asrProvider, Vision: visionProvider}
}

func CacheKey(assetHash, version, provider string, parameters map[string]any) (string, error) {
	b, err := json.Marshal(struct {
		Asset, Version, Provider string
		Parameters               map[string]any
	}{assetHash, version, provider, parameters})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *Service) Analyze(ctx context.Context, req Request) (Result, error) {
	asset, err := s.Store.Asset(req.ProjectID, req.AssetID)
	if err != nil {
		return Result{}, err
	}
	if req.AnalyzerVersion == "" {
		req.AnalyzerVersion = "p2-v1"
	}
	if req.Provider == "" {
		req.Provider = "subtitle"
	}
	parameters := map[string]any{}
	for k, v := range req.Parameters {
		parameters[k] = v
	}
	parameters["_visual"] = req.Visual
	if req.SubtitlePath != "" {
		b, hashErr := os.ReadFile(req.SubtitlePath)
		if hashErr != nil {
			return Result{}, hashErr
		}
		h := sha256.Sum256(b)
		parameters["_subtitle_content_hash"] = hex.EncodeToString(h[:])
	}
	key, err := CacheKey(asset.ContentHash, req.AnalyzerVersion, req.Provider, parameters)
	if err != nil {
		return Result{}, err
	}
	previous, previousErr := s.Store.AnalysisRun(req.ProjectID, req.AssetID, key)
	if previousErr == nil && previous.Status == "completed" {
		return s.resultFromRun(previous)
	}
	run := domain.AnalysisRun{ProjectID: req.ProjectID, AssetID: req.AssetID, AssetContentHash: asset.ContentHash, CacheKey: key, AnalyzerVersion: req.AnalyzerVersion, Provider: req.Provider, Parameters: parameters, Status: "running", Stages: map[string]string{}}
	if _, err = s.Store.PutAnalysisRun(run); err != nil {
		return Result{}, err
	}
	all := []domain.Evidence{}
	if previousErr == nil && previous.Stages["subtitle"] == "completed" {
		priorIDs := map[string]bool{}
		for _, id := range previous.EvidenceIDs {
			priorIDs[id] = true
		}
		// Runs created before stage-level evidence IDs were persisted have an
		// empty list. The matching cache key, provider and analyzer version
		// still make their transcript evidence safe to resume once.
		legacyPartialRun := len(priorIDs) == 0
		if existing, existingErr := s.Store.Evidence(req.ProjectID, []string{req.AssetID}); existingErr == nil {
			for _, e := range existing {
				if (priorIDs[e.ID] || (legacyPartialRun && e.Provider == req.Provider && e.AnalyzerVersion == req.AnalyzerVersion)) && e.Transcript != "" {
					all = append(all, e)
					run.EvidenceIDs = append(run.EvidenceIDs, e.ID)
				}
			}
		}
		if len(all) > 0 {
			run.Stages["subtitle"] = "completed"
		}
	}
	if req.SubtitlePath != "" && run.Stages["subtitle"] != "completed" {
		run.Stages["subtitle"] = "running"
		_, _ = s.Store.PutAnalysisRun(run)
		cues, parseErr := asr.ParseFile(ctx, req.SubtitlePath)
		if parseErr != nil {
			return s.fail(run, "subtitle", parseErr)
		}
		for _, e := range asr.ToEvidence(req.ProjectID, asset, cues, req.Provider, req.AnalyzerVersion, key) {
			if _, err = s.Store.PutEvidence(e); err != nil {
				return s.fail(run, "subtitle", err)
			}
			all = append(all, e)
			run.EvidenceIDs = append(run.EvidenceIDs, e.ID)
		}
		run.Stages["subtitle"] = "completed"
	} else if s.ASR != nil && run.Stages["subtitle"] != "completed" {
		run.Stages["subtitle"] = "running"
		_, _ = s.Store.PutAnalysisRun(run)
		cues, transcribeErr := s.ASR.Transcribe(ctx, asset)
		if transcribeErr != nil {
			return s.fail(run, "subtitle", transcribeErr)
		}
		for _, e := range asr.ToEvidence(req.ProjectID, asset, cues, req.Provider, req.AnalyzerVersion, key) {
			if _, err = s.Store.PutEvidence(e); err != nil {
				return s.fail(run, "subtitle", err)
			}
			all = append(all, e)
			run.EvidenceIDs = append(run.EvidenceIDs, e.ID)
		}
		run.Stages["subtitle"] = "completed"
	} else if run.Stages["subtitle"] != "completed" {
		run.Stages["subtitle"] = "model_unavailable"
	}
	if req.Visual {
		pauses, pauseErr := visual.Pauses(ctx, s.Tools, asset)
		if pauseErr != nil {
			return s.fail(run, "pauses", pauseErr)
		}
		for _, e := range pauses {
			e.ID = cacheEvidenceID(e.ID, key)
			e.CacheKey = key
			if _, err = s.Store.PutEvidence(e); err != nil {
				return s.fail(run, "pauses", err)
			}
			all = append(all, e)
			run.EvidenceIDs = append(run.EvidenceIDs, e.ID)
		}
		run.Stages["pauses"] = "completed"
		run.Stages["visual"] = "running"
		_, _ = s.Store.PutAnalysisRun(run)
		frameDir := filepath.Join(s.Store.Dir, "analysis", asset.ID, key, "frames")
		var frames []domain.Evidence
		var sampleErr error
		if req.Parameters["mode"] == "deep" {
			frames, sampleErr = (visual.Sampler{}).Sample(ctx, s.Tools, asset, frameDir)
		} else {
			frames, sampleErr = visual.Overview(ctx, s.Tools, asset, frameDir)
		}
		if sampleErr != nil {
			return s.fail(run, "visual", sampleErr)
		}
		if s.Vision != nil {
			for start := 0; start < len(frames); start += 8 {
				batch := frames[start:min(start+8, len(frames))]
				missing := []domain.Evidence{}
				existing, _ := s.Store.Evidence(req.ProjectID, []string{req.AssetID})
				cached := map[string]string{}
				for _, e := range existing {
					if e.CacheKey == key {
						cached[e.ID] = e.VisualSummary
					}
				}
				for i := range batch {
					batch[i].VisualSummary = cached[cacheEvidenceID(batch[i].ID, key)]
					if batch[i].VisualSummary == "" {
						missing = append(missing, batch[i])
					}
				}
				if len(missing) > 0 {
					summaries, describeErr := s.Vision.Describe(ctx, missing)
					if describeErr != nil {
						return s.fail(run, "visual", describeErr)
					}
					for i := range batch {
						if batch[i].VisualSummary == "" {
							batch[i].VisualSummary = summaries[batch[i].ID]
						}
					}
				}
				for _, e := range batch {
					e.ID = cacheEvidenceID(e.ID, key)
					e.CacheKey = key
					if _, err = s.Store.PutEvidence(e); err != nil {
						return s.fail(run, "visual", err)
					}
				}
				run.Stages["visual_progress"] = fmt.Sprintf("%d/%d", min(start+8, len(frames)), len(frames))
				_, _ = s.Store.PutAnalysisRun(run)
			}
		}
		for i := range frames {
			frames[i].ID = cacheEvidenceID(frames[i].ID, key)
			frames[i].CacheKey = key
		}
		for _, e := range frames {
			if _, err = s.Store.PutEvidence(e); err != nil {
				return s.fail(run, "visual", err)
			}
			all = append(all, e)
			run.EvidenceIDs = append(run.EvidenceIDs, e.ID)
		}
		run.Stages["visual"] = "completed"
	}
	if len(all) == 0 {
		if existing, existingErr := s.Store.Evidence(req.ProjectID, []string{req.AssetID}); existingErr == nil && len(existing) > 0 {
			all = existing
			run.Stages["indexed"] = "completed"
		}
	}
	if len(all) == 0 {
		return s.fail(run, "analysis", errors.New("model provider unavailable: provide subtitle_path or enable a visual sampler"))
	}
	run.Status = "completed"
	if _, err = s.Store.PutAnalysisRun(run); err != nil {
		return Result{}, err
	}
	return Result{Run: run, Evidence: all}, nil
}

func (s *Service) fail(run domain.AnalysisRun, stage string, err error) (Result, error) {
	run.Status = "failed"
	if errors.Is(err, context.Canceled) {
		run.Status = "cancelled"
	}
	run.Error = err.Error()
	if run.Stages == nil {
		run.Stages = map[string]string{}
	}
	run.Stages[stage] = "failed"
	_, _ = s.Store.PutAnalysisRun(run)
	return Result{Run: run}, err
}

func cacheEvidenceID(id, key string) string {
	if len(key) < 12 {
		return id
	}
	return id + "-" + key[:12]
}

func (s *Service) resultFromRun(run domain.AnalysisRun) (Result, error) {
	items, err := s.Store.Evidence(run.ProjectID, []string{run.AssetID})
	if err != nil {
		return Result{}, err
	}
	allowed := map[string]bool{}
	for _, id := range run.EvidenceIDs {
		allowed[id] = true
	}
	filtered := items[:0]
	for _, e := range items {
		if allowed[e.ID] {
			filtered = append(filtered, e)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].StartUS < filtered[j].StartUS })
	return Result{Run: run, Evidence: filtered}, nil
}

func SubtitlePath(path string) error {
	if path == "" {
		return errors.New("subtitle_path is required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("subtitle path is not a regular file")
	}
	return nil
}
