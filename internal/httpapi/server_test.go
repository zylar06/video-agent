package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zylar06/video-agent/internal/app"
	"github.com/zylar06/video-agent/internal/domain"
)

func TestToolsAreLoopbackAPIJSON(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := httptest.NewServer(New(a))
	defer s.Close()
	resp, err := http.Get(s.URL + "/v1/tools")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("tools response: %s", resp.Status)
	}
	resp.Body.Close()
	resp, err = http.Post(s.URL+"/v1/tools/project_create", "application/json", bytes.NewBufferString(`{"id":"p","name":"P3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create response: %s", resp.Status)
	}
	resp.Body.Close()
	resp, err = http.Get(s.URL + "/v1/artifacts/../../video-agent.db")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path traversal status: %s", resp.Status)
	}
	resp.Body.Close()
}

// The agent tool loop is the only natural-language entry point now; the
// duplicate /v1/chat intent endpoint must not come back.
func TestChatEndpointIsNotServed(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := httptest.NewServer(New(a))
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/chat", "application/json", bytes.NewBufferString(`{"message":"剪成 1 分钟"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The "/" catch-all matches this path for GET only, so a POST arrives as
	// 405 rather than 404. Either way the handler is gone; only 2xx would mean
	// the duplicate endpoint came back.
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("/v1/chat must be gone, got %s", resp.Status)
	}
}

func TestUICreatesAProjectWithoutAnExposedID(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := httptest.NewServer(New(a))
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/ui/projects", "application/json", bytes.NewBufferString(`{"name":"我的首支视频"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create response: %s", resp.Status)
	}
}

func TestUIDraftCanBeEditedAndConfirmed(t *testing.T) {
	t.Setenv("AUTOCLIP_TEXT_BASE_URL", "")
	t.Setenv("AUTOCLIP_TEXT_MODEL", "")
	t.Setenv("AUTOCLIP_TEXT_API_KEY", "")
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Store.CreateProject(domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	asset := domain.MediaAsset{ID: "a", ProjectID: "p", Path: "/fixture.mp4", ContentHash: "hash", DurationUS: 20_000_000, Width: 1280, Height: 720, FPS: "30/1", Status: "ready"}
	if _, err = a.Store.PutAsset(asset); err != nil {
		t.Fatal(err)
	}
	evidence := domain.Evidence{ID: "cue", ProjectID: "p", AssetID: "a", AssetContentHash: "hash", StartUS: 4_000_000, EndUS: 9_000_000, Transcript: "关键结论"}
	if _, err = a.Store.PutEvidence(evidence); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(New(a))
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/ui/proposals", "application/json", bytes.NewBufferString(`{"project_id":"p","asset_id":"a","query":"关键结论","duration_us":5000000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proposal response: %s", resp.Status)
	}
	var created struct {
		OK     bool `json:"ok"`
		Result struct {
			Draft domain.DraftPlan `json:"draft"`
		} `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !created.OK || len(created.Result.Draft.Candidates) != 1 {
		t.Fatalf("draft: %+v", created)
	}
	candidate := created.Result.Draft.Candidates[0]
	edit := `{"base_version":1,"edit":{"candidate_id":"` + candidate.ID + `","kind":"lock"}}`
	resp, err = http.Post(s.URL+"/v1/ui/proposals/"+created.Result.Draft.ID+"/operations", "application/json", bytes.NewBufferString(edit))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit response: %s", resp.Status)
	}
	var edited struct {
		Result domain.DraftPlan `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&edited); err != nil {
		t.Fatal(err)
	}
	if edited.Result.Version != 2 || !edited.Result.Candidates[0].Locked {
		t.Fatalf("edited draft: %+v", edited.Result)
	}
	resp, err = http.Post(s.URL+"/v1/ui/proposals/"+created.Result.Draft.ID+"/confirm", "application/json", bytes.NewBufferString(`{"version":2}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("confirm response: %s", resp.Status)
	}
	var confirmed struct {
		Result domain.TimelineRevision `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&confirmed); err != nil {
		t.Fatal(err)
	}
	if len(confirmed.Result.Items) != 1 || !confirmed.Result.Items[0].Locked {
		t.Fatalf("timeline: %+v", confirmed.Result)
	}
}
