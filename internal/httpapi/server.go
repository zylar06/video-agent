// Package httpapi exposes the same constrained P3 tools over loopback HTTP.
package httpapi

import (
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zylar06/video-agent/internal/agent"
	"github.com/zylar06/video-agent/internal/analysis"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/app"
	"github.com/zylar06/video-agent/internal/chat"
	"github.com/zylar06/video-agent/internal/domain"
)

//go:embed web/index.html
var webFiles embed.FS

//go:embed web/chat.html
var chatPage []byte

// New builds the local HTTP surface. The tool-calling chat routes are wired here
// so the agent runtime reads the same model configuration as the rest of the
// service.
func New(a *app.App) http.Handler {
	s := agent.NewService(a)
	mux := http.NewServeMux()
	agentRoutes(mux, a)
	mux.HandleFunc("GET /chat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(chatPage)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		data, err := webFiles.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "web UI unavailable", http.StatusInternalServerError)
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
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		vision := provider.ConfigFromEnvAliases("VIDEO_AGENT_VISION", "AUTOCLIP_VISION")
		visualEnabled := vision.BaseURL != "" && vision.Model != "" && vision.APIKey != ""
		result, err := analysis.New(a.Store, a.Tools).Analyze(r.Context(), analysis.Request{ProjectID: in.ProjectID, AssetID: in.AssetID, SubtitlePath: in.SubtitlePath, Visual: visualEnabled})
		if err != nil {
			write(w, http.StatusBadRequest, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "model_unavailable", Message: "请上传 SRT/VTT 字幕，或在服务端配置 AUTOCLIP_ASR_* 后重试。"}})
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: result})
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
		draft, err := draftTimeline(a, in.ProjectID, in.AssetID, result)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]any{"reply": result.Reply, "intent": result.Intent, "evidence": result.Evidence, "timeline": draft}})
	})
	mux.HandleFunc("POST /v1/ui/proposals/confirm", func(w http.ResponseWriter, r *http.Request) {
		var timeline domain.TimelineRevision
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&timeline); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		created, err := a.CreateTimeline(timeline)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: created})
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
	switch group {
	case "uploads":
		if ext != ".mp4" && ext != ".mov" && ext != ".m4v" {
			return "", errors.New("仅支持 MP4、MOV 或 M4V 视频")
		}
	case "subtitles":
		if ext != ".srt" && ext != ".vtt" {
			return "", errors.New("字幕仅支持 SRT 或 VTT")
		}
	case "agent-uploads":
		// The chat surfaces accepts both media and subtitles, so it validates
		// against the union rather than one group's format.
		if !isAgentUpload(ext) {
			return "", errors.New("仅支持 MP4、MOV、M4V、SRT 或 VTT")
		}
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

// isAgentUpload reports whether an extension may be dropped into the chat
// surface: media to import, or subtitles to analyze against.
func isAgentUpload(ext string) bool {
	switch ext {
	case ".mp4", ".mov", ".m4v", ".srt", ".vtt":
		return true
	}
	return false
}

func draftTimeline(a *app.App, projectID, assetID string, result chat.Result) (domain.TimelineRevision, error) {
	if len(result.Evidence) == 0 {
		return domain.TimelineRevision{}, errors.New("没有找到可确认的素材片段，请换一种说法或补充字幕")
	}
	asset, err := a.Store.Asset(projectID, assetID)
	if err != nil {
		return domain.TimelineRevision{}, err
	}
	parts := strings.Split(asset.FPS, "/")
	fpsNum, fpsDen := 30, 1
	if len(parts) == 2 {
		if n, e := strconv.Atoi(parts[0]); e == nil && n > 0 {
			fpsNum = n
		}
		if d, e := strconv.Atoi(parts[1]); e == nil && d > 0 {
			fpsDen = d
		}
	}
	t := domain.TimelineRevision{ID: "timeline-" + app.ID(), ProjectID: projectID, Revision: 1, FPSNum: fpsNum, FPSDen: fpsDen, Width: asset.Width, Height: asset.Height}
	remaining := result.Intent.DurationUS
	for i, hit := range result.Evidence {
		e := hit.Evidence
		end := e.EndUS
		if remaining > 0 && end-e.StartUS > remaining {
			end = e.StartUS + remaining
		}
		if end <= e.StartUS {
			break
		}
		t.Items = append(t.Items, domain.ClipItem{ID: "clip-" + strconv.Itoa(i+1), AssetID: e.AssetID, SourceInUS: e.StartUS, SourceOutUS: end, DurationFrames: t.Frames(end - e.StartUS), EvidenceIDs: []string{e.ID}})
		if remaining > 0 {
			remaining -= end - e.StartUS
			if remaining <= 0 {
				break
			}
		}
	}
	if len(t.Items) == 0 {
		return domain.TimelineRevision{}, errors.New("候选片段无法生成时间线")
	}
	t.Reflow()
	return t, nil
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
