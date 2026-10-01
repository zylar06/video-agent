package agent

import (
	"context"
	"encoding/json"
	"github.com/zylar06/video-agent/internal/analysis/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeModel serves a scripted sequence of assistant turns and records every
// request, so a test can assert the exact conversation the loop sent.
type fakeModel struct {
	mu       sync.Mutex
	replies  []string
	requests []map[string]any
	server   *httptest.Server
}

func newFakeModel(t *testing.T, replies ...string) *fakeModel {
	t.Helper()
	f := &fakeModel{replies: replies}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		f.mu.Lock()
		f.requests = append(f.requests, decoded)
		index := len(f.requests) - 1
		var reply string
		if index < len(f.replies) {
			reply = f.replies[index]
		} else {
			reply = `{"choices":[{"finish_reason":"stop","message":{"content":"没有更多步骤"}}]}`
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeModel) runner() Runner {
	return Runner{Model: provider.OpenAIText{Config: provider.Config{BaseURL: f.server.URL, Model: "fake", APIKey: "k", HTTPClient: f.server.Client()}}}
}

func (f *fakeModel) lastRequest(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("model was never called")
	}
	return f.requests[len(f.requests)-1]
}

func collect() (*[]Event, Emit) {
	var events []Event
	return &events, func(e Event) { events = append(events, e) }
}

// A plain answer must terminate in one step with no tool traffic.
func TestTurnAnswersWithoutTools(t *testing.T) {
	model := newFakeModel(t, `{"choices":[{"finish_reason":"stop","message":{"content":"我可以帮你剪。"}}]}`)
	history := NewHistory()
	events, emit := collect()

	if err := model.runner().Turn(context.Background(), history, "你能做什么", emit); err != nil {
		t.Fatalf("turn: %v", err)
	}
	kinds := kindsOf(*events)
	if kinds[len(kinds)-1] != EventDone {
		t.Fatalf("turn must end with done, got %v", kinds)
	}
	if contains(kinds, EventToolStart) {
		t.Fatalf("no tool should have run: %v", kinds)
	}
	view := history.View()
	if len(view) != 2 || view[0].Role != "user" || view[1].Role != "assistant" {
		t.Fatalf("transcript wrong: %+v", view)
	}
}

// The core lifecycle: model asks for a tool, the result is fed back, model
// answers. Assert the follow-up request really carried the assistant tool call
// and the tool result.
func TestTurnRunsToolThenAnswers(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"unknown_probe","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"已经查完了。"}}]}`,
	)
	history := NewHistory()
	events, emit := collect()

	// A real Service with a nil App cannot dispatch, but the loop only needs the
	// tool to be *attempted* and its failure surfaced back to the model.
	runner := model.runner()
	runner.Tools = &Service{}
	runner.System = "你是本地视频剪辑助手。"
	err := runner.Turn(context.Background(), history, "帮我看看", emit)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}

	kinds := kindsOf(*events)
	for _, want := range []EventKind{EventStep, EventToolStart, EventToolEnd, EventText, EventDone} {
		if !contains(kinds, want) {
			t.Fatalf("missing %s in %v", want, kinds)
		}
	}

	var toolEnd Event
	for _, e := range *events {
		if e.Kind == EventToolEnd {
			toolEnd = e
		}
	}
	if toolEnd.Succeeded {
		t.Fatal("an undispatchable tool must be reported as a failure")
	}
	if !json.Valid([]byte(toolEnd.Result)) {
		t.Fatalf("tool result must stay valid JSON for the model: %s", toolEnd.Result)
	}

	// Second request must replay system + assistant tool call + matching tool message.
	second := model.lastRequest(t)
	messages, _ := second["messages"].([]any)
	if len(messages) != 4 {
		// system + user + assistant(tool_calls) + tool
		t.Fatalf("expected 4 messages in follow-up, got %d: %+v", len(messages), messages)
	}
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("system prompt must lead the request: %+v", messages[0])
	}
	assistant := messages[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("third message must be the assistant turn: %+v", assistant)
	}
	if _, ok := assistant["tool_calls"]; !ok {
		t.Fatalf("assistant tool call was dropped: %+v", assistant)
	}
	toolMsg := messages[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" {
		t.Fatalf("tool result not paired to its call: %+v", toolMsg)
	}
}

func TestTurnOffersToolsToTheModel(t *testing.T) {
	model := newFakeModel(t, `{"choices":[{"message":{"content":"好"}}]}`)
	history := NewHistory()
	_, emit := collect()

	runner := model.runner()
	runner.Tools = &Service{}
	if err := runner.Turn(context.Background(), history, "hi", emit); err != nil {
		t.Fatalf("turn: %v", err)
	}
	tools, ok := model.lastRequest(t)["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools were not offered to the model")
	}
	if len(tools) != len((&Service{}).ToolSpecs()) {
		t.Fatalf("expected every declared tool, got %d", len(tools))
	}
}

// A model that keeps calling tools must be stopped by the step ceiling rather
// than looping forever.
func TestTurnStopsAtMaxSteps(t *testing.T) {
	endless := `{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"c","type":"function","function":{"name":"unknown_probe","arguments":"{}"}}]}}]}`
	model := newFakeModel(t, endless, endless, endless, endless, endless)
	history := NewHistory()
	events, emit := collect()

	runner := model.runner()
	runner.Tools = &Service{}
	runner.MaxSteps = 3
	if err := runner.Turn(context.Background(), history, "一直循环", emit); err != nil {
		t.Fatalf("hitting the ceiling is not an error: %v", err)
	}
	steps := 0
	for _, e := range *events {
		if e.Kind == EventStep {
			steps++
		}
	}
	if steps != 3 {
		t.Fatalf("expected exactly 3 steps, got %d", steps)
	}
	last := (*events)[len(*events)-1]
	if last.Kind != EventDone || !strings.Contains(last.Text, "上限") {
		t.Fatalf("expected a ceiling notice, got %+v", last)
	}
}

func TestTurnReportsModelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	runner := Runner{Model: provider.OpenAIText{Config: provider.Config{BaseURL: server.URL, Model: "m", APIKey: "k", HTTPClient: server.Client()}}}
	history := NewHistory()
	events, emit := collect()

	err := runner.Turn(context.Background(), history, "hi", emit)
	if err == nil {
		t.Fatal("a failed model call must surface as an error")
	}
	if (*events)[len(*events)-1].Kind != EventError {
		t.Fatalf("expected a terminal error event, got %+v", *events)
	}
}

func TestTurnRejectsEmptyInput(t *testing.T) {
	runner := newFakeModel(t).runner()
	if err := runner.Turn(context.Background(), NewHistory(), "   ", nil); err == nil {
		t.Fatal("blank message must be rejected")
	}
	if err := runner.Turn(context.Background(), nil, "hi", nil); err == nil {
		t.Fatal("nil history must be rejected")
	}
}

func TestHistoryIsolateSystemPromptAndCopies(t *testing.T) {
	h := NewHistory()
	h.Append(Message{Role: "user", Content: "一"})
	h.Append(Message{Role: "assistant", Content: "二"})

	withSystem := h.Messages("你是剪辑助手")
	if len(withSystem) != 3 || withSystem[0].Role != "system" {
		t.Fatalf("system prompt not prepended: %+v", withSystem)
	}
	if len(h.Messages("")) != 2 {
		t.Fatal("empty system prompt must not be added")
	}

	// Callers must not be able to mutate stored history through the view.
	view := h.View()
	view[0].Text = "mutated"
	if h.View()[0].Text != "一" {
		t.Fatal("View must return a copy")
	}
	if h.Len() != 2 {
		t.Fatalf("unexpected history length %d", h.Len())
	}
}

func kindsOf(events []Event) []EventKind {
	out := make([]EventKind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func contains(kinds []EventKind, want EventKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
