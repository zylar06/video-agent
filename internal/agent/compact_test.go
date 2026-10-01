package agent

import (
	"strings"
	"testing"
)

// buildExchange appends one complete user -> assistant(tool_call) -> tool cycle.
func buildExchange(h *History, ask, toolName string) {
	h.Append(Message{Role: "user", Content: ask})
	h.Append(Message{Role: "assistant", Content: "我来查一下。", ToolCalls: []ToolCall{
		{ID: "call-" + ask, Type: "function", Function: ToolCallFunc{Name: toolName, Arguments: "{}"}},
	}})
	h.Append(Message{Role: "tool", ToolCallID: "call-" + ask, Name: toolName, Content: `{"ok":true,"result":"` + strings.Repeat("x", 200) + `"}`})
	h.Append(Message{Role: "assistant", Content: "查到了。"})
}

// The invariant that keeps requests valid: every tool message must still have its
// originating assistant tool_call, and vice versa. A provider rejects the request
// otherwise, so compaction must never break the pairing.
func TestCompactKeepsToolCallsPaired(t *testing.T) {
	h := NewHistory()
	for i := 0; i < 12; i++ {
		buildExchange(h, "请求"+string(rune('A'+i)), "project_list")
	}

	before := len(h.MessagesCopy())
	result := h.Compact(10)
	if !result.Replaced {
		t.Fatalf("expected compaction to run: %+v", result)
	}

	messages := h.MessagesCopy()
	if len(messages) != result.Kept {
		t.Fatalf("Kept=%d but history holds %d", result.Kept, len(messages))
	}
	if len(messages) >= before {
		t.Fatalf("compaction did not shrink history: %d -> %d", before, len(messages))
	}

	// Collect the ids present on each side.
	called := map[string]bool{}
	answered := map[string]bool{}
	for _, m := range messages {
		for _, call := range m.ToolCalls {
			called[call.ID] = true
		}
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
			if !called[m.ToolCallID] {
				t.Fatalf("tool result %q has no surviving assistant tool_call", m.ToolCallID)
			}
		}
	}
	for id := range called {
		if !answered[id] {
			t.Fatalf("assistant tool_call %q has no surviving result", id)
		}
	}
}

func TestCompactKeepsRecentExchangesVerbatim(t *testing.T) {
	h := NewHistory()
	for i := 0; i < 10; i++ {
		buildExchange(h, "请求"+string(rune('A'+i)), "project_list")
	}
	h.Compact(8)

	messages := h.MessagesCopy()
	// The newest exchange must still be intact.
	last := messages[len(messages)-1]
	if last.Content != "查到了。" {
		t.Fatalf("newest assistant reply was altered: %+v", last)
	}
	var foundAsk bool
	for _, m := range messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, "请求I") {
			foundAsk = true
		}
	}
	if !foundAsk {
		t.Fatal("the most recent user request must remain verbatim")
	}
}

func TestCompactSummarizesPriorAsks(t *testing.T) {
	h := NewHistory()
	for i := 0; i < 10; i++ {
		buildExchange(h, "请求"+string(rune('A'+i)), "project_list")
	}
	h.Compact(6)

	messages := h.MessagesCopy()
	if messages[0].Role != "assistant" {
		t.Fatalf("summary must be the first message, got %q", messages[0].Role)
	}
	summary := messages[0].Content
	if !strings.Contains(summary, "压缩") {
		t.Fatalf("summary should say the history was compacted: %q", summary)
	}
	// The earliest ask must survive as intent even though its payload is gone.
	if !strings.Contains(summary, "请求A") {
		t.Fatalf("summary dropped the user's earlier intent: %q", summary)
	}
	// The model must be told to re-verify rather than trust stale details.
	if !strings.Contains(summary, "重新调用") {
		t.Fatalf("summary should tell the model to re-check: %q", summary)
	}
}

func TestCompactLeavesShortHistoryAlone(t *testing.T) {
	h := NewHistory()
	buildExchange(h, "唯一请求", "search")
	before := h.MessagesCopy()

	result := h.Compact(24)
	if result.Replaced {
		t.Fatalf("short history must not be compacted: %+v", result)
	}
	if len(h.MessagesCopy()) != len(before) {
		t.Fatal("short history was modified")
	}
	if h.NeedsCompaction(24) {
		t.Fatal("short history reported as needing compaction")
	}
}

// A history that already starts at a user message within the window must be left
// alone rather than truncated mid-exchange.
func TestCompactIsIdempotent(t *testing.T) {
	h := NewHistory()
	for i := 0; i < 12; i++ {
		buildExchange(h, "请求"+string(rune('A'+i)), "project_list")
	}
	h.Compact(8)
	afterFirst := len(h.MessagesCopy())

	h.Compact(8)
	if got := len(h.MessagesCopy()); got > afterFirst {
		t.Fatalf("second pass grew history: %d -> %d", afterFirst, got)
	}
	// Pairing must still hold after repeated passes.
	called := map[string]bool{}
	for _, m := range h.MessagesCopy() {
		for _, call := range m.ToolCalls {
			called[call.ID] = true
		}
	}
	for _, m := range h.MessagesCopy() {
		if m.Role == "tool" && !called[m.ToolCallID] {
			t.Fatalf("second pass orphaned a tool result: %+v", m)
		}
	}
}

func TestNeedsCompactionTracksWindow(t *testing.T) {
	h := NewHistory()
	for i := 0; i < 5; i++ {
		buildExchange(h, "请求"+string(rune('A'+i)), "search")
	}
	if !h.NeedsCompaction(4) {
		t.Fatal("20 messages should exceed a 4-message window")
	}
	if h.NeedsCompaction(100) {
		t.Fatal("20 messages should fit a 100-message window")
	}
}
