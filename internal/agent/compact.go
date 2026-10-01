package agent

import (
	"strconv"
	"strings"
)

// Long conversations cannot fit a context window, and a video-editing session
// grows fast: one analyze call can return hundreds of evidence rows. Compaction
// keeps the recent exchanges verbatim and reduces everything older to a short
// note, so the model still knows what has been done without carrying every tool
// payload for the rest of the session.
//
// The constraint that makes this delicate: an assistant turn requesting tools and
// the tool results answering it must be dropped together. Providers reject a tool
// message whose originating tool_call is missing, and conversely an assistant
// tool_call with no result. Compaction therefore only ever removes complete
// exchanges, never individual messages.

// DefaultMaxHistoryMessages is the verbatim window. Older exchanges collapse
// into a summary.
const DefaultMaxHistoryMessages = 24

// Compaction is the outcome of one compaction pass.
type Compaction struct {
	Removed  int // model messages dropped
	Kept     int // model messages still verbatim
	Replaced bool
}

// Compact reduces the model-facing history in place to at most maxMessages.
//
// The cut always lands on a user message, which guarantees the retained tail is
// a sequence of complete exchanges. Everything before the cut is replaced by one
// assistant note naming the user requests that are no longer verbatim, so the
// model does not silently forget what it was asked to do.
func (h *History) Compact(maxMessages int) Compaction {
	if maxMessages <= 0 {
		maxMessages = DefaultMaxHistoryMessages
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.messages) <= maxMessages {
		return Compaction{Kept: len(h.messages)}
	}

	// Walk back from the end until the window is full, then extend backwards to
	// the nearest user message so no exchange is split.
	start := len(h.messages) - maxMessages
	for start > 0 && h.messages[start].Role != "user" {
		start--
	}
	if start == 0 {
		// The window already begins at a user turn; nothing to compact.
		return Compaction{Kept: len(h.messages)}
	}

	dropped := h.messages[:start]
	asks := userAsks(dropped)
	summary := Message{Role: "assistant", Content: summarize(dropped, asks)}

	kept := make([]Message, 0, len(h.messages)-start+1)
	kept = append(kept, summary)
	kept = append(kept, h.messages[start:]...)
	result := Compaction{Removed: len(dropped), Kept: len(kept), Replaced: true}
	h.messages = kept
	return result
}

// userAsks collects what the user actually requested, in order. Those are the
// durable intent; tool payloads are reproducible by calling the tools again.
func userAsks(messages []Message) []string {
	var asks []string
	for _, m := range messages {
		if m.Role != "user" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		asks = append(asks, truncate(strings.Join(strings.Fields(m.Content), " "), 60))
	}
	return asks
}

func summarize(dropped []Message, asks []string) string {
	var b strings.Builder
	b.WriteString("【早期对话已压缩】为节省上下文，此前 ")
	b.WriteString(strconv.Itoa(len(dropped)))
	b.WriteString(" 条消息已折叠。")
	if len(asks) > 0 {
		b.WriteString("用户先后提出过这些要求：")
		for i, ask := range asks {
			if i > 0 {
				b.WriteString("；")
			}
			b.WriteString(ask)
		}
		b.WriteString("。")
	}
	b.WriteString("这些步骤的结果已不在上下文中；如需具体数据（素材 id、证据时间戳、时间线 revision 等），请重新调用相应工具确认后再下结论。")
	return b.String()
}

// NeedsCompaction reports whether the history exceeds the verbatim window.
func (h *History) NeedsCompaction(maxMessages int) bool {
	if maxMessages <= 0 {
		maxMessages = DefaultMaxHistoryMessages
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.messages) > maxMessages
}
