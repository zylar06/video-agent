package httpapi

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zylar06/video-agent/internal/app"
)

func TestIsAgentUploadAcceptsMediaAndSubtitles(t *testing.T) {
	for _, ext := range []string{".mp4", ".mov", ".m4v", ".srt", ".vtt"} {
		if !isAgentUpload(ext) {
			t.Errorf("%s should be accepted by the chat upload", ext)
		}
	}
	for _, ext := range []string{".txt", ".zip", ".png", "", ".SRT"} {
		// .SRT is listed deliberately: saveUpload lowercases before calling, so
		// the raw uppercase form must not be treated as valid here.
		if isAgentUpload(ext) {
			t.Errorf("%s should not be accepted", ext)
		}
	}
}

// The chat upload shares saveUpload with the classic page but needs a wider
// format set. If the groups ever collapse into one rule, one of the two surfaces
// breaks silently.
func TestSaveUploadGroupsEnforceTheirOwnFormats(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		group, name string
		wantErr     bool
	}{
		{"uploads", "clip.mp4", false},
		{"uploads", "clip.srt", true},
		{"subtitles", "clip.srt", false},
		{"subtitles", "clip.mp4", true},
		{"agent-uploads", "clip.mp4", false},
		{"agent-uploads", "clip.srt", false},
		{"agent-uploads", "clip.txt", true},
	}
	for _, tc := range cases {
		path, err := saveUpload(dir, tc.group, tc.name, strings.NewReader("x"))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s/%s: expected rejection, got %s", tc.group, tc.name, path)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s/%s: unexpected error %v", tc.group, tc.name, err)
			continue
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s/%s: file not written: %v", tc.group, tc.name, statErr)
		}
		if filepath.Ext(path) != filepath.Ext(tc.name) {
			t.Errorf("%s/%s: extension not preserved in %s", tc.group, tc.name, path)
		}
	}
}

// A dropped file must come back with a path the agent can be told about, and a
// kind the client uses to word its message correctly.
func TestAgentUploadReturnsPathAndKind(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	defer a.Close()
	handler := New(a)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "clip.mp4")
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	_, _ = part.Write([]byte("not really a video"))
	if err := form.Close(); err != nil {
		t.Fatalf("close form: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/agent/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Path string `json:"path"`
			Name string `json:"name"`
			Kind string `json:"kind"`
			Size int64  `json:"size"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if !envelope.OK {
		t.Fatalf("upload reported failure: %s", rec.Body.String())
	}
	if envelope.Result.Kind != "video" {
		t.Errorf("kind = %q, want video", envelope.Result.Kind)
	}
	if envelope.Result.Name != "clip.mp4" {
		t.Errorf("name = %q", envelope.Result.Name)
	}
	if _, err := os.Stat(envelope.Result.Path); err != nil {
		t.Errorf("returned path does not exist: %v", err)
	}
	// The path must be absolute, because the agent passes it straight to
	// assets_import which resolves it against the server process.
	if !filepath.IsAbs(envelope.Result.Path) {
		t.Errorf("path must be absolute, got %q", envelope.Result.Path)
	}
}

func TestAgentUploadRejectsUnsupportedFormat(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	defer a.Close()
	handler := New(a)

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "notes.txt")
	_, _ = part.Write([]byte("hello"))
	_ = form.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/agent/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected rejection, got 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "MP4") {
		t.Errorf("rejection should say which formats are allowed: %s", rec.Body.String())
	}
}

func TestAgentUploadRequiresAFile(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	defer a.Close()
	handler := New(a)

	req := httptest.NewRequest(http.MethodPost, "/v1/agent/upload", strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("empty upload must fail: %s", rec.Body.String())
	}
}
