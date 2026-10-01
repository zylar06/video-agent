package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/zylar06/video-agent/internal/app"
)

func newTestRunner(t *testing.T) Runner {
	t.Helper()
	a, err := app.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return Runner{Tools: NewService(a)}
}

// Writes must never be classified as concurrent: two interleaved writers would
// make revision numbers and generated ids nondeterministic.
func TestConcurrencyPolicyKeepsWritersSerial(t *testing.T) {
	runner := newTestRunner(t)
	for _, name := range []string{"project_list", "assets_list", "search", "timeline_get", "jobs_list", "jobs_get"} {
		if !runner.concurrencySafe(name) {
			t.Errorf("%s only observes and should be concurrency-safe", name)
		}
	}
	for _, name := range []string{"project_create", "assets_import", "analyze", "timeline_create", "edit_apply", "render_submit", "jobs_cancel"} {
		if runner.concurrencySafe(name) {
			t.Errorf("%s writes and must not be concurrency-safe", name)
		}
	}
	// An unknown tool cannot be classified, so it must not race.
	if runner.concurrencySafe("definitely_not_a_tool") {
		t.Error("an unknown tool must be treated as unsafe")
	}
	if (&Runner{}).concurrencySafe("project_list") {
		t.Error("a runner without a tool service must not report anything safe")
	}
}

// Many calls in one turn must come back paired and in the model's original
// order, whatever order they completed in. A shuffled list makes the replay
// harder for the model to follow.
func TestDispatchPreservesCallOrderAndPairing(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"jobs_list","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"project_list","arguments":"{}"}},
			{"id":"c3","type":"function","function":{"name":"jobs_list","arguments":"{}"}},
			{"id":"c4","type":"function","function":{"name":"project_list","arguments":"{}"}}
		]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"查完了。"}}]}`,
	)
	history := NewHistory()
	events, emit := collect()

	runner := model.runner()
	runner.Tools = newTestRunner(t).Tools
	if err := runner.Turn(context.Background(), history, "看看有什么", emit); err != nil {
		t.Fatalf("turn: %v", err)
	}

	// Every call must be started and settled exactly once.
	started := map[string]int{}
	ended := map[string]int{}
	for _, e := range *events {
		switch e.Kind {
		case EventToolStart:
			started[e.ToolCallID]++
		case EventToolEnd:
			ended[e.ToolCallID]++
		}
	}
	if len(started) != 4 || len(ended) != 4 {
		t.Fatalf("expected 4 distinct calls started and ended, got %d/%d", len(started), len(ended))
	}
	for id, n := range started {
		if n != 1 {
			t.Errorf("call %s started %d times", id, n)
		}
		if ended[id] != 1 {
			t.Errorf("call %s ended %d times", id, ended[id])
		}
	}

	// The tool results must appear in the model's order.
	messages := history.MessagesCopy()
	var toolIDs []string
	for _, m := range messages {
		if m.Role == "tool" {
			toolIDs = append(toolIDs, m.ToolCallID)
		}
	}
	want := []string{"c1", "c2", "c3", "c4"}
	if strings.Join(toolIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("tool results out of order: got %v want %v", toolIDs, want)
	}
}

// The transcript view must stay consistent with the model history even when the
// calls ran concurrently.
func TestDispatchRecordsEveryToolInTranscript(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[
			{"id":"c1","type":"function","function":{"name":"jobs_list","arguments":"{}"}},
			{"id":"c2","type":"function","function":{"name":"project_list","arguments":"{}"}}
		]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"好了。"}}]}`,
	)
	history := NewHistory()
	_, emit := collect()

	runner := model.runner()
	runner.Tools = newTestRunner(t).Tools
	if err := runner.Turn(context.Background(), history, "并行查一下", emit); err != nil {
		t.Fatalf("turn: %v", err)
	}

	var tools int
	for _, v := range history.View() {
		if v.Role == "tool" {
			tools++
			if v.ToolName == "" {
				t.Errorf("transcript tool entry without a name: %+v", v)
			}
			if !json.Valid([]byte(v.Result)) {
				t.Errorf("transcript result is not JSON: %s", v.Result)
			}
		}
	}
	if tools != 2 {
		t.Fatalf("expected 2 tool entries in the transcript, got %d", tools)
	}
}

// Result assembly must not depend on completion order, which is only guaranteed
// if the results slice is indexed rather than appended.
func TestDispatchResultsAreIndexAddressed(t *testing.T) {
	runner := newTestRunner(t)
	calls := []ToolCall{
		{ID: "slow", Function: ToolCallFunc{Name: "jobs_list", Arguments: "{}"}},
		{ID: "fast", Function: ToolCallFunc{Name: "project_list", Arguments: "{}"}},
	}
	history := NewHistory()

	var mu sync.Mutex
	var events []Event
	runner.dispatch(context.Background(), history, 1, calls, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	})

	messages := history.MessagesCopy()
	if len(messages) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(messages))
	}
	if messages[0].ToolCallID != "slow" || messages[1].ToolCallID != "fast" {
		t.Fatalf("results not index-addressed: %s, %s", messages[0].ToolCallID, messages[1].ToolCallID)
	}
}

// A turn mixing reads and a write must still settle everything exactly once,
// with the write serialized after the reads that preceded it.
func TestDispatchMixedReadsAndWrite(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[
			{"id":"r1","type":"function","function":{"name":"jobs_list","arguments":"{}"}},
			{"id":"r2","type":"function","function":{"name":"project_list","arguments":"{}"}},
			{"id":"w1","type":"function","function":{"name":"project_create","arguments":"{\"id\":\"p-mixed\",\"name\":\"混合\"}"}},
			{"id":"r3","type":"function","function":{"name":"project_list","arguments":"{}"}}
		]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"完成。"}}]}`,
	)
	history := NewHistory()
	events, emit := collect()
	runner := model.runner()
	runner.Tools = newTestRunner(t).Tools

	if err := runner.Turn(context.Background(), history, "并行混合", emit); err != nil {
		t.Fatalf("turn: %v", err)
	}
	for _, id := range []string{"r1", "r2", "w1", "r3"} {
		var ends int
		for _, e := range *events {
			if e.Kind == EventToolEnd && e.ToolCallID == id {
				ends++
			}
		}
		if ends != 1 {
			t.Errorf("call %s settled %d times, want 1", id, ends)
		}
	}
	// The write must have succeeded and be visible.
	var writeOK bool
	for _, e := range *events {
		if e.Kind == EventToolEnd && e.ToolCallID == "w1" {
			writeOK = e.Succeeded
		}
	}
	if !writeOK {
		t.Fatal("the write call should have succeeded")
	}
}
