package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/zylar06/video-agent/internal/agent"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"github.com/zylar06/video-agent/internal/app"
)

// agentRoutes wires the tool-calling chat surface: a streaming turn endpoint, a
// session list, and a transcript replay so a reloaded page resumes the
// conversation instead of starting over.
func agentRoutes(mux *http.ServeMux, a *app.App) *agent.Runtime {
	runtime := agent.NewRuntime(a, provider.OpenAIText{Config: provider.ConfigFromEnvAliases("VIDEO_AGENT_TEXT", "AUTOCLIP_TEXT")})

	mux.HandleFunc("GET /v1/agent/sessions", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: runtime.Sessions()})
	})
	mux.HandleFunc("GET /v1/agent/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: runtime.Transcript(r.PathValue("id"))})
	})
	mux.HandleFunc("DELETE /v1/agent/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := runtime.DeleteSession(r.PathValue("id")); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]string{"deleted": r.PathValue("id")}})
	})
	mux.HandleFunc("GET /v1/agent/config", func(w http.ResponseWriter, r *http.Request) {
		config := provider.ConfigFromEnvAliases("VIDEO_AGENT_TEXT", "AUTOCLIP_TEXT")
		// The key is never echoed; only whether a model is usable.
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]any{
			"model":         config.Model,
			"configured":    config.BaseURL != "" && config.Model != "" && config.APIKey != "",
			"max_steps":     runtime.MaxSteps,
			"tool_count":    len(agent.NewService(a).ToolSpecs()),
			"system_prompt": runtime.System,
		}})
	})
	mux.HandleFunc("POST /v1/agent/chat", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SessionID string `json:"session_id"`
			Message   string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		if strings.TrimSpace(in.Message) == "" {
			write(w, http.StatusBadRequest, invalid(errors.New("message is required")))
			return
		}
		if strings.TrimSpace(in.SessionID) == "" {
			in.SessionID = "session-" + app.ID()
		}
		streamTurn(w, r, runtime, in.SessionID, in.Message)
	})
	// Upload stores a dropped file and returns its local path. The client then
	// hands that path to the agent in a normal message, so an import goes through
	// the same tool the model already knows about instead of a side channel that
	// the conversation cannot see.
	mux.HandleFunc("POST /v1/agent/upload", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			write(w, http.StatusBadRequest, invalid(errors.New("文件超过 2GB 或上传格式不正确")))
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			write(w, http.StatusBadRequest, invalid(errors.New("请选择要上传的文件")))
			return
		}
		defer file.Close()
		path, err := saveUpload(a.Store.Dir, "agent-uploads", header.Filename, file)
		if err != nil {
			write(w, http.StatusBadRequest, invalid(err))
			return
		}
		write(w, http.StatusOK, agent.Envelope{APIVersion: agent.APIVersion, OK: true, Result: map[string]any{
			"path": path,
			"name": header.Filename,
			"size": header.Size,
			"kind": uploadKind(header.Filename),
		}})
	})
	return runtime
}

// uploadKind classifies an upload so the client can word its message correctly:
// subtitles are attached to an analysis, video is imported as an asset.
func uploadKind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".vtt":
		return "subtitle"
	case ".mp4", ".mov", ".m4v":
		return "video"
	default:
		return "unknown"
	}
}

// streamTurn runs one agent turn and streams its events as Server-Sent Events.
// The response is committed before the turn starts so the client can render
// progress; failures after that point arrive as an `error` event rather than an
// HTTP status, because the status line is already sent.
func streamTurn(w http.ResponseWriter, r *http.Request, runtime *agent.Runtime, sessionID, message string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		write(w, http.StatusInternalServerError, agent.Envelope{APIVersion: agent.APIVersion, OK: false, Error: &agent.APIError{Code: "stream_unsupported", Message: "服务器不支持流式响应"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Announce the session first so a client that had no id can adopt this one.
	sendEvent(w, flusher, "session", map[string]any{"session_id": sessionID})

	// A heartbeat keeps intermediaries from closing an idle stream while the
	// model is thinking.
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			}
		}
	}()

	emitted := false
	err := runtime.Run(r.Context(), sessionID, message, func(event agent.Event) {
		emitted = true
		sendEvent(w, flusher, "agent", event)
	})

	switch {
	case err != nil && r.Context().Err() != nil:
		sendEvent(w, flusher, "agent", agent.Event{Kind: agent.EventError, Text: "已取消"})
	case err != nil && !emitted:
		// Nothing streamed yet, so a plain HTTP error is still useful to the client.
		sendEvent(w, flusher, "agent", agent.Event{Kind: agent.EventError, Text: err.Error()})
	}
	sendEvent(w, flusher, "end", map[string]any{"session_id": sessionID})
}

func sendEvent(w io.Writer, flusher http.Flusher, name string, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(`{"kind":"error","text":"事件序列化失败"}`)
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, encoded)
	flusher.Flush()
}
