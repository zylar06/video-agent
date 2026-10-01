package agent

import (
	"testing"

	"github.com/zylar06/video-agent/internal/app"
	"github.com/zylar06/video-agent/internal/analysis/provider"
)

func TestRuntimeRestoresSessionFromStore(t *testing.T) {
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r := NewRuntime(a, provider.OpenAIText{})
	s := r.Session("session-persisted")
	s.History.Append(provider.Message{Role: "user", Content: "找出数学片段"})
	s.History.RecordTool("project_list", "{}", `{"ok":true}`, true)
	r.persist(s)

	reloaded := NewRuntime(a, provider.OpenAIText{})
	views := reloaded.Transcript("session-persisted")
	if len(views) != 2 || views[0].Text != "找出数学片段" || views[1].ToolName != "project_list" {
		t.Fatalf("restored transcript: %+v", views)
	}
	items := reloaded.Sessions()
	if len(items) != 1 || items[0].ID != "session-persisted" || items[0].Messages != 1 {
		t.Fatalf("restored summaries: %+v", items)
	}
}
