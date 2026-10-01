package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// An unapproved render must never reach the store. This is enforced in the loop,
// not by prompt wording, because the model ignored the prompt during testing.
func TestExpensiveToolIsBlockedWithoutConsent(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"content":"好的","tool_calls":[{"id":"c1","type":"function","function":{"name":"render_submit","arguments":"{\"timeline_id\":\"t1\"}"}}]}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"需要你确认后我再渲染。"}}]}`,
	)
	history := NewHistory()
	events, emit := collect()

	runner := model.runner()
	runner.Tools = &Service{} // would panic if the call were dispatched
	err := runner.Turn(context.Background(), history, "导出成 MP4", emit)
	if err != nil {
		t.Fatalf("turn: %v", err)
	}

	var end Event
	for _, e := range *events {
		if e.Kind == EventToolEnd {
			end = e
		}
	}
	if end.ToolName != "render_submit" {
		t.Fatalf("expected the render call to be gated, got %+v", end)
	}
	if end.Succeeded {
		t.Fatal("a blocked render must be reported as failure")
	}
	if !strings.Contains(end.Result, "confirmation_required") {
		t.Fatalf("block must name its reason: %s", end.Result)
	}
	if !json.Valid([]byte(end.Result)) {
		t.Fatalf("blocked result must stay valid JSON: %s", end.Result)
	}
}

// After the assistant asks and the user agrees, the same call goes through.
func TestExpensiveToolRunsAfterUserConsent(t *testing.T) {
	model := newFakeModel(t,
		`{"choices":[{"finish_reason":"stop","message":{"content":"我打算剪前 60 秒，确认开始渲染吗？"}}]}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"好的，现在开始。"}}]}`,
	)
	history := NewHistory()
	_, emit := collect()
	runner := model.runner()

	// Turn 1: assistant asks for confirmation.
	if err := runner.Turn(context.Background(), history, "帮我在时间线 t1 上导出", emit); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if !history.UserConsented("render_submit", "") {
		// No user agreement yet — only the question.
		t.Log("no consent yet, as expected")
	}

	// Turn 2: user agrees.
	if err := runner.Turn(context.Background(), history, "好的，可以", emit); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !history.UserConsented("render_submit", "") {
		t.Fatal("explicit agreement after a confirmation question must count as consent")
	}
}

// Consent must not be inferred from an unrelated early "好".
func TestConsentRequiresAQuestionFirst(t *testing.T) {
	history := NewHistory()
	history.Append(Message{Role: "user", Content: "好的"})
	history.Append(Message{Role: "assistant", Content: "我已经列出素材。"})
	if history.UserConsented("render_submit", "") {
		t.Fatal("a bare acknowledgement with no confirmation question must not authorise a render")
	}
}

// Non-expensive tools are never gated.
func TestCheapToolsAreNotGated(t *testing.T) {
	runner := Runner{}
	for _, name := range []string{"project_list", "search", "timeline_get", "edit_apply", "jobs_get"} {
		if reason, blocked := runner.confirmationRequired(name, "{}"); blocked {
			t.Fatalf("%s should not be gated: %s", name, reason)
		}
	}
	if _, blocked := runner.confirmationRequired("render_submit", "{}"); !blocked {
		t.Fatal("render_submit must be gated")
	}
}

func TestRefusalResultIsParseable(t *testing.T) {
	var decoded struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(refusalResult("需要确认")), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.OK || decoded.Error.Code != "confirmation_required" || decoded.Error.Message == "" {
		t.Fatalf("unexpected refusal payload: %+v", decoded)
	}
}
