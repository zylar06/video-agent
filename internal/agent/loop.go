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
	"github.com/zylar06/video-agent/internal/domain"
)

// Message and its constructors are aliases for the domain types. They live in
// the domain layer so the session store can persist a conversation without the
// provider and store packages importing each other.
type (
	Message      = domain.Message
	ToolCall     = domain.ToolCall
	ToolCallFunc = domain.ToolCallFunc
)

func ToolMessage(callID, name, content string) Message {
	return domain.ToolMessage(callID, name, content)
}

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
	EventCompacted EventKind = "compacted"  // older history was folded to fit
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
	// MaxParallel bounds concurrent read-only tool calls in one turn. Zero uses
	// DefaultMaxParallelToolCalls.
	MaxParallel int // MaxHistory is the verbatim message window; older exchanges are folded.
	// Zero uses DefaultMaxHistoryMessages.
	MaxHistory int
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

	// Fold older history before building the request. A long editing session
	// accumulates tool payloads far faster than prose, and an over-full context
	// fails the whole turn. Compact is a no-op when the window already fits.
	maxHistory := r.MaxHistory
	if maxHistory <= 0 {
		maxHistory = DefaultMaxHistoryMessages
	}
	if compacted := history.Compact(maxHistory); compacted.Replaced {
		emit(Event{Kind: EventCompacted, Text: "为节省上下文，已折叠 " + strconv.Itoa(compacted.Removed) + " 条早期消息。"})
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

		r.dispatch(ctx, history, step, result.ToolCalls, emit)
	}

	// Running out of steps is a normal outcome the user should understand, not a
	// crash: the transcript already holds everything done so far.
	message := "已达到本轮工具调用上限（" + strconv.Itoa(maxSteps) + " 步），先停在这里。你可以继续让我接着做。"
	emit(Event{Kind: EventDone, Step: maxSteps, Text: message})
	return nil
}

// DefaultMaxParallelToolCalls bounds how many observing calls run at once. A
// model can emit many calls in one turn, and running them serially wastes the
// user's time; running all of them unbounded would swamp a local SQLite store.
const DefaultMaxParallelToolCalls = 4

// dispatch runs one turn's tool calls. Read-only calls run concurrently because
// they cannot race on project state; every call that writes runs alone, in the
// order the model listed it.
//
// Results are appended to history in the model's original order regardless of
// completion order, because the transcript is what the next request replays and
// a shuffled tool_calls list is harder for the model to follow.
func (r Runner) dispatch(ctx context.Context, history *History, step int, calls []ToolCall, emit Emit) {
	if len(calls) <= 1 {
		for _, call := range calls {
			outcome := r.runCall(ctx, call, emit, step)
			history.Append(ToolMessage(call.ID, call.Function.Name, outcome.output))
			history.RecordTool(call.Function.Name, call.Function.Arguments, outcome.output, outcome.ok)
		}
		return
	}

	// emit may be called from several goroutines now, so serialize it.
	var emitMu sync.Mutex
	safeEmit := func(event Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emit(event)
	}

	type pending struct {
		call    ToolCall
		outcome callOutcome
	}
	results := make([]pending, len(calls))
	limit := r.MaxParallel
	if limit <= 0 {
		limit = DefaultMaxParallelToolCalls
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, call := range calls {
		if !r.concurrencySafe(call.Function.Name) {
			// A writing call waits for everything already in flight, then runs
			// alone, so revision numbers stay deterministic.
			wg.Wait()
			results[i] = pending{call: call, outcome: r.runCall(ctx, call, safeEmit, step)}
			continue
		}
		wg.Add(1)
		go func(i int, call ToolCall) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = pending{call: call, outcome: callOutcome{output: cancellationResult(), ok: false}}
				return
			}
			results[i] = pending{call: call, outcome: r.runCall(ctx, call, safeEmit, step)}
		}(i, call)
	}
	wg.Wait()

	for i, result := range results {
		history.Append(ToolMessage(calls[i].ID, result.call.Function.Name, result.outcome.output))
		history.RecordTool(result.call.Function.Name, result.call.Function.Arguments, result.outcome.output, result.outcome.ok)
	}
}

type callOutcome struct {
	output string
	ok     bool
}

// runCall executes a single call, applying the confirmation gate first.
func (r Runner) runCall(ctx context.Context, call ToolCall, emit Emit, step int) callOutcome {
	emit(Event{Kind: EventToolStart, Step: step, ToolCallID: call.ID, ToolName: call.Function.Name, Arguments: call.Function.Arguments})
	output, ok := func() (string, bool) {
		if reason, blocked := r.confirmationRequired(call.Function.Name, call.Function.Arguments); blocked {
			return refusalResult(reason), false
		}
		return r.Tools.RunTool(ctx, call.Function.Name, call.Function.Arguments)
	}()
	emit(Event{Kind: EventToolEnd, Step: step, ToolCallID: call.ID, ToolName: call.Function.Name, Result: output, Succeeded: ok})
	return callOutcome{output: output, ok: ok}
}

// concurrencySafe reports whether a tool may run alongside others. An unknown
// tool is treated as unsafe: it cannot be classified, so it must not race.
func (r Runner) concurrencySafe(name string) bool {
	if r.Tools == nil {
		return false
	}
	for _, spec := range r.Tools.ToolSpecs() {
		if spec.Name == name {
			return spec.ConcurrencySafe()
		}
	}
	return false
}

func cancellationResult() string {
	return `{"ok":false,"error":{"code":"cancelled","message":"本轮已取消，该工具未执行。"}}`
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

// SessionSink persists a conversation as it grows, so an interrupted turn keeps
// everything already shown to the user.
type SessionSink interface {
	AppendMessage(view View) error
	SaveState(messages []Message) error
}

// History is an ordered, concurrency-safe conversation transcript. It stores the
// model-visible messages plus a UI-facing view so a reloaded session can be
// replayed without re-deriving tool output.
type History struct {
	mu       sync.Mutex
	messages []Message
	view     []View
	sink     SessionSink
}

// View is the durable transcript a client renders on reconnect. It is an alias
// for the domain type so this package keeps naming it while the store can
// persist it without importing this package (which would be a cycle).
type View = domain.View

func NewHistory() *History { return &History{} }

// AttachSink starts persisting this conversation. Entries already present are
// flushed first so a resumed session is not truncated on disk.
func (h *History) AttachSink(sink SessionSink) {
	h.mu.Lock()
	existing := append([]View(nil), h.view...)
	h.sink = sink
	h.mu.Unlock()
	if sink == nil {
		return
	}
	for _, v := range existing {
		_ = sink.AppendMessage(v)
	}
}

// SetRestored installs history loaded from storage.
func (h *History) SetRestored(messages []Message, view []View) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append([]Message(nil), messages...)
	h.view = append([]View(nil), view...)
}

// MessagesCopy returns the model-facing history, used to persist it.
func (h *History) MessagesCopy() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.messages...)
}

// appendViewLocked records a renderable entry and hands it to the sink.
func (h *History) appendViewLocked(view View) {
	h.view = append(h.view, view)
	if h.sink != nil {
		_ = h.sink.AppendMessage(view)
	}
}

func (h *History) Append(message Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, message)
	switch message.Role {
	case "user":
		h.appendViewLocked(View{Role: "user", Text: message.Content})
	case "assistant":
		if message.Content != "" {
			h.appendViewLocked(View{Role: "assistant", Text: message.Content})
		}
	}
}

// RecordTool adds the UI view of a settled tool call. Kept separate from Append
// because tool results enter model history and the transcript through different
// paths.
func (h *History) RecordTool(name, arguments, result string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.appendViewLocked(View{Role: "tool", ToolName: name, Arguments: arguments, Result: result, Succeeded: ok})
}

// Messages returns the request payload: an optional system prompt followed by the
// full transcript.
func (h *History) Messages(system string) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Message, 0, len(h.messages)+1)
	if strings.TrimSpace(system) != "" {
		out = append(out, Message{Role: "system", Content: system})
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
