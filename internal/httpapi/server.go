// Package httpapi exposes the same constrained P3 tools over loopback HTTP.
package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zylar06/video-agent/internal/agent"
	"github.com/zylar06/video-agent/internal/analysis"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/app"
	"github.com/zylar06/video-agent/internal/chat"
	"github.com/zylar06/video-agent/internal/domain"
	"github.com/zylar06/video-agent/internal/planner"
	"github.com/zylar06/video-agent/internal/store"
)

//go:embed web/chat.html
var webFiles embed.FS

type uiAnalysisTask struct {
	store              *store.Store
	projectID, assetID string
	mu                 sync.RWMutex
	cancel             context.CancelFunc
	status             string
	result             analysis.Result
	err                string
}

func (t *uiAnalysisTask) snapshot() map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()
	run := t.result.Run
	if t.status == "running" && t.store != nil {
		runs, _ := t.store.AnalysisRuns(t.projectID, t.assetID)
		for _, r := range runs {
			if r.UpdatedAt.After(run.UpdatedAt) {
				run = r
			}
		}
	}
	return map[string]any{"status": t.status, "run": run, "evidence": t.result.Evidence, "error": t.err}
}

func New(a *app.App) http.Handler {
	s := agent.NewService(a)
	var analysisTasksMu sync.RWMutex
	analysisTasks := map[string]*uiAnalysisTask{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webFiles.ReadFile("web/chat.html")
		if err != nil {
			http.Error(w, "chat UI unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]string{"status": "ok"}})
	})
	mux.HandleFunc("GET /v1/tools", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: s.Names()})
	})
	mux.HandleFunc("POST /v1/chat", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var input chat.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&input); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		result, err := (chat.Service{Store: a.Store, Text: provider.OpenAIText{Config: provider.ConfigFromEnvAliases("VIDEO_AGENT_TEXT", "AUTOCLIP_TEXT")}, Analyzer: analysis.New(a.Store, a.Tools)}).Handle(r.Context(), input)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: result})
	})
	// The UI routes are deliberately local implementation details. They provide
	// a safe browser workflow without exposing filesystem paths or tool JSON.
	mux.HandleFunc("POST /v1/ui/projects", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Name) == "" {
			write(w, http.StatusBadRequest, invalid(errors.New("请输入项目名称")))
			return
		}
		p := domain.Project{ID: "project-" + app.ID(), Name: strings.TrimSpace(in.Name)}
		if err := a.Store.CreateProject(p); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: p})
	})
	mux.HandleFunc("GET /v1/ui/projects/{id}/assets", func(w http.ResponseWriter, r *http.Request) {
		assets, err := a.Store.Assets(r.PathValue("id"))
		if err != nil {
			write(w, http.StatusNotFound, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: assets})
	})
	mux.HandleFunc("POST /v1/ui/projects/{id}/assets", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			write(w, http.StatusBadRequest, invalid(errors.New("视频文件超过 2GB 或上传格式不正确")))
			return
		}
		file, header, err := r.FormFile("video")
		if err != nil {
			write(w, http.StatusBadRequest, invalid(errors.New("请选择视频文件")))
			return
		}
		defer file.Close()
		path, err := saveUpload(a.Store.Dir, "uploads", header.Filename, file)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		asset, err := a.Tools.Import(r.Context(), a.Store, r.PathValue("id"), path)
		_ = os.Remove(path)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		result := map[string]any{"asset": asset}
		if subtitle, sh, subtitleErr := r.FormFile("subtitle"); subtitleErr == nil {
			defer subtitle.Close()
			sp, saveErr := saveUpload(a.Store.Dir, "subtitles", sh.Filename, subtitle)
			if saveErr != nil {
				write(w, http.StatusBadRequest, invalid(saveErr))
				return
			}
			result["subtitle_path"] = sp
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: result})
	})
	mux.HandleFunc("POST /v1/ui/analyze", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ProjectID    string `json:"project_id"`
			AssetID      string `json:"asset_id"`
			SubtitlePath string `json:"subtitle_path,omitempty"`
			Mode         string `json:"mode,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		asrConfig := provider.ConfigFromEnvAliases("VIDEO_AGENT_ASR", "AUTOCLIP_ASR")
		if in.Mode == "" {
			in.Mode = "overview"
		}
		if in.Mode != "overview" && in.Mode != "deep" {
			write(w, http.StatusBadRequest, invalid(errors.New("mode must be overview or deep")))
			return
		}
		visionConfig := provider.ConfigFromEnvAliases("VIDEO_AGENT_VISION", "AUTOCLIP_VISION")
		visualEnabled := visionConfig.BaseURL != "" && visionConfig.Model != "" && visionConfig.APIKey != ""
		if in.SubtitlePath == "" && (asrConfig.BaseURL == "" || asrConfig.Model == "" || asrConfig.APIKey == "") && !visualEnabled {
			write(w, http.StatusBadRequest, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "model_unavailable", Message: "请上传 SRT/VTT 字幕，或在服务端配置 AUTOCLIP_ASR_* / AUTOCLIP_VISION_* 后重试。"}})
			return
		}
		taskID := "analysis-" + app.ID()
		ctx, cancel := context.WithCancel(context.Background())
		task := &uiAnalysisTask{cancel: cancel, status: "queued", store: a.Store, projectID: in.ProjectID, assetID: in.AssetID}
		analysisTasksMu.Lock()
		analysisTasks[taskID] = task
		analysisTasksMu.Unlock()
		go func() {
			task.mu.Lock()
			task.status = "running"
			task.mu.Unlock()
			defer cancel()
			result, err := analysis.New(a.Store, a.Tools).Analyze(ctx, analysis.Request{ProjectID: in.ProjectID, AssetID: in.AssetID, SubtitlePath: in.SubtitlePath, AnalyzerVersion: "layered-v1", Provider: "fusion", Visual: visualEnabled, Parameters: map[string]any{"mode": in.Mode}})
			task.mu.Lock()
			defer task.mu.Unlock()
			task.result = result
			if err != nil {
				task.err = err.Error()
				if errors.Is(err, context.Canceled) {
					task.status = "cancelled"
				} else {
					task.status = "failed"
				}
				return
			}
			task.status = "completed"
		}()
		write(w, http.StatusAccepted, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]string{"id": taskID, "status": "queued"}})
	})
	mux.HandleFunc("GET /v1/ui/analysis/{id}", func(w http.ResponseWriter, r *http.Request) {
		analysisTasksMu.RLock()
		task := analysisTasks[r.PathValue("id")]
		analysisTasksMu.RUnlock()
		if task == nil {
			write(w, http.StatusNotFound, invalid(errors.New("analysis task not found")))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: task.snapshot()})
	})
	mux.HandleFunc("POST /v1/ui/analysis/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		analysisTasksMu.RLock()
		task := analysisTasks[r.PathValue("id")]
		analysisTasksMu.RUnlock()
		if task == nil {
			write(w, http.StatusNotFound, invalid(errors.New("analysis task not found")))
			return
		}
		task.mu.RLock()
		cancel := task.cancel
		task.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: task.snapshot()})
	})
	mux.HandleFunc("POST /v1/ui/proposals", func(w http.ResponseWriter, r *http.Request) {
		var in chat.Request
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		result, err := (chat.Service{Store: a.Store, Text: provider.OpenAIText{Config: provider.ConfigFromEnvAliases("VIDEO_AGENT_TEXT", "AUTOCLIP_TEXT")}}).Handle(r.Context(), in)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		if result.Intent.DurationUS <= 0 {
			write(w, http.StatusUnprocessableEntity, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "invalid_request", Message: "请说明目标时长，例如“剪成 60 秒”。"}})
			return
		}
		draft, err := (planner.Service{Store: a.Store}).Create("draft-"+app.ID(), in.ProjectID, in.AssetID, result.Intent.Query, result.Intent.DurationUS, result.Evidence)
		if err == nil {
			draft, err = analysis.New(a.Store, a.Tools).Refine(r.Context(), draft)
		}
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]any{"reply": result.Reply, "intent": result.Intent, "draft": draft}})
	})
	mux.HandleFunc("GET /v1/ui/proposals/{id}", func(w http.ResponseWriter, r *http.Request) {
		draft, err := a.Store.Draft(r.PathValue("id"))
		if err != nil {
			write(w, http.StatusNotFound, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: draft})
	})
	mux.HandleFunc("POST /v1/ui/proposals/{id}/operations", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BaseVersion int          `json:"base_version"`
			Edit        planner.Edit `json:"edit"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		draft, err := (planner.Service{Store: a.Store}).Apply(r.PathValue("id"), in.BaseVersion, in.Edit)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				write(w, http.StatusConflict, invalid(err))
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				write(w, http.StatusNotFound, invalid(err))
				return
			}
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: draft})
	})
	mux.HandleFunc("POST /v1/ui/proposals/{id}/confirm", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int `json:"version"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		draft, err := a.Store.Draft(r.PathValue("id"))
		if err != nil {
			write(w, http.StatusNotFound, invalid(err))
			return
		}
		if draft.Version != in.Version {
			write(w, http.StatusConflict, invalid(store.ErrConflict))
			return
		}
		asset, err := a.Store.Asset(draft.ProjectID, draft.AssetID)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		timeline, err := planner.Timeline(draft, asset)
		if err == nil {
			err = a.Store.ConfirmDraft(draft, timeline)
		}
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: timeline})
	})
	mux.HandleFunc("POST /v1/ui/proposals/confirm", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusGone, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "invalid_request", Message: "请通过草案确认接口提交已审阅的候选片段。"}})
	})
	mux.HandleFunc("POST /v1/tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		result := s.Call(r.Context(), r.PathValue("name"), body)
		write(w, status(result), result)
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := a.Store.Job(r.PathValue("id"))
		if err != nil {
			write(w, http.StatusNotFound, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "not_found", Message: err.Error()}})
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: j})
	})
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{"id": r.PathValue("id")})
		result := s.Call(r.Context(), "jobs_cancel", body)
		write(w, status(result), result)
	})
	mux.HandleFunc("GET /v1/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, err := a.Store.Job(r.PathValue("id"))
		if err != nil || j.Status != "completed" {
			write(w, http.StatusNotFound, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "not_found", Message: "completed artifact not found"}})
			return
		}
		base := filepath.Join(a.Store.Dir, "exports")
		rel, err := filepath.Rel(base, j.Output)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			write(w, http.StatusForbidden, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "invalid_request", Message: "artifact is outside managed exports"}})
			return
		}
		f, err := os.Open(j.Output)
		if err != nil {
			write(w, http.StatusNotFound, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "not_found", Message: "artifact file not found"}})
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			write(w, http.StatusNotFound, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "not_found", Message: "artifact file not found"}})
			return
		}
		http.ServeContent(w, r, filepath.Base(j.Output), info.ModTime(), f)
	})
	return securityHeaders(mux)
}

func saveUpload(dataDir, group, name string, source io.Reader) (string, error) {
	ext := strings.ToLower(filepath.Ext(name))
	if group == "uploads" && ext != ".mp4" && ext != ".mov" && ext != ".m4v" {
		return "", errors.New("仅支持 MP4、MOV 或 M4V 视频")
	}
	if group == "subtitles" && ext != ".srt" && ext != ".vtt" {
		return "", errors.New("字幕仅支持 SRT 或 VTT")
	}
	dir := filepath.Join(dataDir, group)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "upload-*"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, copyErr := io.Copy(f, source)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return "", errors.Join(copyErr, closeErr)
	}
	return path, nil
}

func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 60 * time.Second}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
func invalid(err error) agent.Envelope {
	return agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "invalid_request", Message: err.Error()}}
}
func status(e agent.Envelope) int {
	if e.OK {
		return http.StatusOK
	}
	if e.Error != nil && e.Error.Code == "not_found" {
		return http.StatusNotFound
	}
	if e.Error != nil && e.Error.Code == "revision_conflict" {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}
func write(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
