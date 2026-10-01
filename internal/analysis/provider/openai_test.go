package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zylar06/video-agent/internal/analysis/asr"
	"github.com/zylar06/video-agent/internal/domain"
)

func TestOpenAITranscriberAndVision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/audio/transcriptions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"segments": []any{map[string]any{"start": 1.25, "end": 2.5, "text": "关键时刻"}}})
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "主持人正在讲话"}}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	video := filepath.Join(dir, "video.mp4")
	if err := os.WriteFile(video, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BaseURL: server.URL, Model: "test", APIKey: "test-key", HTTPClient: server.Client()}
	cues, err := (OpenAITranscriber{Config: cfg}).Transcribe(context.Background(), domain.MediaAsset{Path: video, DurationUS: 5_000_000})
	if err != nil || len(cues) != 1 || cues[0].StartUS != 1_250_000 || cues[0].EndUS != 2_500_000 {
		t.Fatalf("transcription: %+v, %v", cues, err)
	}
	image := filepath.Join(dir, "frame.jpg")
	if err := os.WriteFile(image, []byte("jpeg"), 0600); err != nil {
		t.Fatal(err)
	}
	descriptions, err := (OpenAIVision{Config: cfg}).Describe(context.Background(), []domain.Evidence{{ID: "frame-1", FrameRefs: []string{image}}})
	if err == nil || len(descriptions) != 0 {
		t.Fatalf("vision: %+v, %v", descriptions, err)
	}
}

func TestParseQwenAudioSSE(t *testing.T) {
	raw := "event: result\ndata: {\"output\":{\"sentence\":{\"sentence_id\":1,\"sentence_end\":true,\"begin_time\":100,\"end_time\":1500,\"text\":\"第一句\"}}}\n\ndata: {\"output\":{\"sentence\":{\"sentence_id\":1,\"sentence_end\":true,\"begin_time\":100,\"end_time\":1500,\"text\":\"第一句\"}}}\n"
	cues, err := parseQwenAudioSSE(strings.NewReader(raw))
	if err != nil || len(cues) != 1 || cues[0].StartUS != 100_000 || cues[0].EndUS != 1_500_000 {
		t.Fatalf("cues: %+v, %v", cues, err)
	}
}

func TestParseQwenAudioJSON(t *testing.T) {
	cue, err := parseQwenAudioJSON([]byte(`{"output":{"sentence":{"sentence_id":3,"sentence_end":true,"begin_time":100,"end_time":1500,"text":"第一句"}}}`))
	if err != nil || cue.Index != 3 || cue.StartUS != 100_000 || cue.EndUS != 1_500_000 || cue.Text != "第一句" {
		t.Fatalf("cue: %+v, %v", cue, err)
	}
	_, err = parseQwenAudioJSON(bytes.TrimSpace([]byte(`{"output":{}}`)))
	if err == nil {
		t.Fatal("expected malformed response error")
	}
}

func TestNormalizeCumulativeQwenCues(t *testing.T) {
	got := normalizeCumulativeQwenCues([]asr.Cue{
		{Index: 1, StartUS: 360_000, EndUS: 5_920_000, Text: "第一句。"},
		{Index: 2, StartUS: 360_000, EndUS: 24_080_000, Text: "第一句。第二句。"},
		{Index: 3, StartUS: 360_000, EndUS: 34_200_000, Text: "第一句。第二句。第三句。"},
	})
	if len(got) != 3 || got[0].StartUS != 360_000 || got[0].EndUS != 5_920_000 || got[1].StartUS != 5_920_000 || got[1].EndUS != 24_080_000 || got[1].Text != "第二句。" || got[2].StartUS != 24_080_000 || got[2].Text != "第三句。" {
		t.Fatalf("normalized cues: %+v", got)
	}
}

func TestVisionBatchesMultipleFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "{\"frames\":[{\"id\":0,\"summary\":\"开场\"},{\"id\":1,\"summary\":\"人物走动\"}]}"}}}})
	}))
	defer server.Close()
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one.jpg"), filepath.Join(dir, "two.jpg")
	if err := os.WriteFile(one, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (OpenAIVision{Config: Config{BaseURL: server.URL, Model: "vision", APIKey: "key", HTTPClient: server.Client()}}).Describe(context.Background(), []domain.Evidence{{ID: "a", FrameRefs: []string{one}}, {ID: "b", FrameRefs: []string{two}}})
	if err != nil || got["a"] != "开场" || got["b"] != "人物走动" {
		t.Fatalf("batch: %+v, %v", got, err)
	}
}

func TestVisionBatchRejectsMissingFrame(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content := `{"frames":[{"id":0,"summary":"开场"}]}`
		if calls == 2 {
			content = "人物走动"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer server.Close()
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one.jpg"), filepath.Join(dir, "two.jpg")
	if err := os.WriteFile(one, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (OpenAIVision{Config: Config{BaseURL: server.URL, Model: "vision", APIKey: "key", HTTPClient: server.Client()}}).Describe(context.Background(), []domain.Evidence{{ID: "a", FrameRefs: []string{one}}, {ID: "b", FrameRefs: []string{two}}})
	if err == nil || calls != 2 || len(got) != 0 {
		t.Fatalf("neighbor context: calls=%d result=%+v err=%v", calls, got, err)
	}
}

func TestVisionBatchRejectsUnstructuredResponse(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		content := "这一批画面正在讲解数学问题"
		if calls == 2 {
			content = "第一帧"
		}
		if calls == 3 {
			content = "第二帧"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer server.Close()
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one.jpg"), filepath.Join(dir, "two.jpg")
	if err := os.WriteFile(one, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := (OpenAIVision{Config: Config{BaseURL: server.URL, Model: "vision", APIKey: "key", HTTPClient: server.Client()}}).Describe(context.Background(), []domain.Evidence{{ID: "a", FrameRefs: []string{one}}, {ID: "b", FrameRefs: []string{two}}})
	if err == nil || calls != 2 || len(got) != 0 {
		t.Fatalf("batch context: calls=%d result=%+v err=%v", calls, got, err)
	}
}
