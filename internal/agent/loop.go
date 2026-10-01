// The agent lifecycle for video editing: ask the model, run whichever tools it
// chooses, feed the results back, repeat until it answers. Tool choice is the
// model's; this file only enforces the boundary (only registered tools run,
// every call is bounded and observable).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/zylar06/video-agent/internal/analysis/provider"
)

// DefaultMaxSteps bounds how many model round-trips one user message may spend.
// A tool loop without a ceiling is a runaway cost bug.
const DefaultMaxSteps = 12

// EventKind enumerates what the UI reacts to while a turn is running.
type EventKind string

const (
	EventText      EventKind = "text"       // assistant prose for this turn
	EventToolStart EventKind = "tool_start" // a tool call was chosen
	EventToolEnd   EventKind = "tool_end"   // a tool call settled
	EventStep      EventKind = "step"       // step boundary, for progress display
	EventError     EventKind = "error"      // the turn ended in failure
	EventDone      EventKind = "done"       // the turn finished
)

// Event is one observable step of a turn. Emit must never be nil.
type Event struct {
	Kind EventKind `json:"kind"`
	Step int       `json:"step"`
	// Text carries assistant prose for EventText and a message for EventError.
	Text string `json:"text,omitempty"`
	// Tool fields are set for tool_start/tool_end.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	Succeeded  bool   `json:"succeeded,omitempty"`
}

// Emit delivers one event. Implementations must be safe for sequential calls
// from the loop goroutine; the loop never calls Emit concurrently.
type Emit func(Event)

// Runner drives one conversation. History is owned by the caller so sessions can
// be persisted and resumed.
type Runner struct {
	Model    provider.OpenAIText
	Tools    *Service
	System   string
	MaxSteps int
	// Consent answers "has the user already agreed to this specific expensive
	// action?". Nil means nothing is pre-approved, so gated tools always ask.
	Consent func(toolName, arguments string) bool
}

// Turn appends the user message, then loops model -> tools until the model
// answers without requesting more tools. It always emits a terminal event.
func (r Runner) Turn(ctx context.Context, history *History, userMessage string, emit Emit) error {
	if emit == nil {
		emit = func(Event) {}
	}
	if strings.TrimSpace(userMessage) == "" {
		return errors.New("message is required")
	}
	if history == nil {
		return errors.New("history is required")
	}
	history.Append(provider.Message{Role: "user", Content: userMessage})

	maxSteps := r.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}

	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			emit(Event{Kind: EventError, Step: step, Text: "已取消"})
			return err
		}
		emit(Event{Kind: EventStep, Step: step})

		result, err := r.Model.Chat(ctx, history.Messages(r.System), r.modelTools())
		if err != nil {
			emit(Event{Kind: EventError, Step: step, Text: err.Error()})
			return err
		}

		// Keep the assistant turn (with its tool calls) in history before the
		// results, otherwise the provider rejects the following tool messages.
		history.Append(result.AssistantMessage())
		if result.Content != "" {
			emit(Event{Kind: EventText, Step: step, Text: result.Content})
		}

		if !result.WantsTools() {
			emit(Event{Kind: EventDone, Step: step, Text: result.Content})
			return nil
		}

		for _, call := range result.ToolCalls {
			emit(Event{Kind: EventToolStart, Step: step, ToolCallID: call.ID, ToolName: call.Function.Name, Arguments: call.Function.Arguments})
			var output string
			var ok bool
			if reason, blocked := r.confirmationRequired(call.Function.Name, call.Function.Arguments); blocked {
				output, ok = refusalResult(reason), false
			} else {
				output, ok = r.Tools.RunTool(ctx, call.Function.Name, call.Function.Arguments)
			}
			emit(Event{Kind: EventToolEnd, Step: step, ToolCallID: call.ID, ToolName: call.Function.Name, Result: output, Succeeded: ok})
			history.Append(provider.ToolMessage(call.ID, call.Function.Name, output))
			history.RecordTool(call.Function.Name, call.Function.Arguments, output, ok)
		}
	}

	// Running out of steps is a normal outcome the user should understand, not a
	// crash: the transcript already holds everything done so far.
	message := "已达到本轮工具调用上限（" + strconv.Itoa(maxSteps) + " 步），先停在这里。你可以继续让我接着做。"
	emit(Event{Kind: EventDone, Step: maxSteps, Text: message})
	return nil
}

// modelTools tolerates an unconfigured tool service so a pure-chat composition
// still runs without declaring tools.
func (r Runner) modelTools() []provider.ToolDefinition {
	if r.Tools == nil {
		return nil
	}
	return r.Tools.ModelTools()
}

// expensiveTools spend real time and write artifacts the user has not agreed to
// yet. A prompt instruction alone does not reliably stop a model from calling
// them — it rendered unasked during testing — so the gate is enforced here.
var expensiveTools = map[string]string{
	"render_submit": "渲染会真实消耗时间并写出文件，需要用户先明确同意",
}

// confirmationRequired reports whether a call must wait for explicit consent.
// Consent is satisfied by the user's own wording in this turn, or by them
// answering a previous confirmation question.
func (r Runner) confirmationRequired(name, arguments string) (string, bool) {
	reason, gated := expensiveTools[name]
	if !gated {
		return "", false
	}
	if r.Consent != nil && r.Consent(name, arguments) {
		return "", false
	}
	return reason, true
}

func refusalResult(reason string) string {
	encoded, err := json.Marshal(map[string]any{
		"api_version": "v1",
		"ok":          false,
		"error": map[string]string{
			"code":    "confirmation_required",
			"message": reason + "。请先用一两句话说明打算剪成什么样，等用户确认后再调用本工具。",
		},
	})
	if err != nil {
		return `{"ok":false,"error":{"code":"confirmation_required"}}`
	}
	return string(encoded)
}

// History is an ordered, concurrency-safe conversation transcript. It stores the
// model-visible messages plus a UI-facing view so a reloaded session can be
// replayed without re-deriving tool output.
type History struct {
	mu       sync.Mutex
	messages []provider.Message
	view     []View
}

// View is the durable transcript a client renders on reconnect.
type View struct {
	Role      string `json:"role"`
	Text      string `json:"text,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Succeeded bool   `json:"succeeded,omitempty"`
}

// HistorySnapshot is the durable form of a conversation. It contains both
// provider messages (needed to continue the model turn) and the compact view
// rendered by the browser (needed to replay tool cards without recomputing).
type HistorySnapshot struct {
	Version  int                `json:"version"`
	Messages []provider.Message `json:"messages"`
	View     []View             `json:"view"`
}

func NewHistory() *History { return &History{} }

func (h *History) Snapshot() HistorySnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HistorySnapshot{Version: 1, Messages: append([]provider.Message(nil), h.messages...), View: append([]View(nil), h.view...)}
}

func (h *History) Restore(snapshot HistorySnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append([]provider.Message(nil), snapshot.Messages...)
	h.view = append([]View(nil), snapshot.View...)
}

func (h *History) Append(message provider.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, message)
	switch message.Role {
	case "user":
		h.view = append(h.view, View{Role: "user", Text: message.Content})
	case "assistant":
		if message.Content != "" {
			h.view = append(h.view, View{Role: "assistant", Text: message.Content})
		}
	}
}

// RecordTool adds the UI view of a settled tool call. Kept separate from Append
// because tool results enter model history and the transcript through different
// paths.
func (h *History) RecordTool(name, arguments, result string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.view = append(h.view, View{Role: "tool", ToolName: name, Arguments: arguments, Result: result, Succeeded: ok})
}

// Messages returns the request payload: an optional system prompt followed by the
// full transcript.
func (h *History) Messages(system string) []provider.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]provider.Message, 0, len(h.messages)+1)
	if strings.TrimSpace(system) != "" {
		out = append(out, provider.Message{Role: "system", Content: system})
	}
	return append(out, h.messages...)
}

// View returns a copy of the renderable transcript.
func (h *History) View() []View {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]View(nil), h.view...)
}

// Len reports how many model messages are held, used to decide compaction.
func (h *History) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.messages)
}

// Consent markers. The assistant asks before an expensive step; the user then
// agrees. Consent only counts once a question has actually been asked, so a
// bare "好" in an unrelated context cannot authorise a render.
var (
	confirmationAsked = []string{"确认", "是否", "要不要", "需要我", "可以吗", "同意", "开始渲染", "开始导出"}
	affirmative       = []string{"好的", "好，", "好!", "好！", "可以", "确认", "同意", "没问题", "行", "是的", "开始吧", "渲染吧", "导出吧", "就这样"}
)

// UserConsented reports whether the user has agreed to an expensive action in
// this conversation. It answers the narrow question "was a confirmation
// requested, and did the user say yes afterwards?" rather than trying to infer
// intent from a single message.
func (h *History) UserConsented(_, _ string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	asked := false
	for _, v := range h.view {
		switch v.Role {
		case "assistant":
			if v.Text != "" && containsAny(v.Text, confirmationAsked) {
				asked = true
			}
		case "user":
			if asked && containsAny(v.Text, affirmative) {
				return true
			}
		}
	}
	return false
}

func containsAny(text string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// Describe formats one tool event for logging or a fallback UI.
func Describe(event Event) string {
	switch event.Kind {
	case EventToolStart:
		return fmt.Sprintf("调用 %s %s", event.ToolName, compact(event.Arguments))
	case EventToolEnd:
		return fmt.Sprintf("%s 返回 %s", event.ToolName, compact(event.Result))
	default:
		return event.Text
	}
}

// compact shortens a JSON payload for one-line display without parsing it.
func compact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 120 {
		return s
	}
	return s[:117] + "..."
}

// MarshalView keeps the transcript JSON-friendly for persistence.
func MarshalView(views []View) ([]byte, error) { return json.Marshal(views) }
