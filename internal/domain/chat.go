package domain

// ToolCall is one function invocation requested by a model. Arguments stays raw
// JSON: the tool owns validation, and re-encoding here would only lose fidelity.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type,omitempty"`
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one turn of an OpenAI-compatible conversation. It carries the union
// of the fields the tool-calling loop needs: plain user/assistant text, an
// assistant turn that requested tools, and the tool results answering it.
//
// It lives in the domain layer because both the provider and the session store
// must name it, and either importing the other would be a cycle.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolMessage wraps one tool result for the follow-up request. Providers reject a
// tool result whose originating call is absent, so the id must be preserved.
func ToolMessage(callID, name, content string) Message {
	return Message{Role: "tool", ToolCallID: callID, Name: name, Content: content}
}

// AssistantMessage rebuilds an assistant turn so it can be replayed into the next
// request together with any tool calls it requested.
func AssistantMessage(content string, calls []ToolCall) Message {
	return Message{Role: "assistant", Content: content, ToolCalls: calls}
}


// View is one renderable entry of a conversation transcript. It lives in the
// domain layer because both the agent loop and the session store depend on it,
// and having either import the other would be a cycle.
//
// It is deliberately separate from the model-facing message: the model needs
// tool call ids and roles, while a UI needs a tool's name, arguments and result
// laid out as a card. Keeping one struct for both would force the transcript to
// carry provider bookkeeping it never displays.
type View struct {
	Role      string `json:"role"`
	Text      string `json:"text,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Succeeded bool   `json:"succeeded,omitempty"`
}
