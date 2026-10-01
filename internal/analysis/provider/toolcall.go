package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/zylar06/video-agent/internal/domain"
)

// Message, ToolCall and ToolCallFunc are aliases for the domain types. They live
// there so the session store can persist a conversation without the provider and
// store packages importing each other.
type (
	Message      = domain.Message
	ToolCall     = domain.ToolCall
	ToolCallFunc = domain.ToolCallFunc
)

// ToolDefinition is the model-facing half of a registered tool. Only name,
// description and parameters are ever sent; execution metadata stays local.
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatResult is one assistant turn: either prose, or tool calls to run next.
type ChatResult struct {
	Content   string
	ToolCalls []ToolCall
	Finish    string
	// Usage is passed through for token accounting when the provider reports it.
	Usage map[string]any
}

// WantsTools reports whether the model asked for work rather than answering.
func (r ChatResult) WantsTools() bool { return len(r.ToolCalls) > 0 }

// Chat performs one completion with optional tool definitions. It is the only
// model call the agent loop needs: prose when no tool is required, tool calls
// when the model decides to act.
func (p OpenAIText) Chat(ctx context.Context, messages []Message, tools []ToolDefinition) (ChatResult, error) {
	if p.Config.BaseURL == "" || p.Config.Model == "" || p.Config.APIKey == "" {
		return ChatResult{}, errors.New("model provider unavailable: text provider is not configured")
	}
	if len(messages) == 0 {
		return ChatResult{}, errors.New("chat requires at least one message")
	}
	payload := map[string]any{"model": p.Config.Model, "temperature": 0, "messages": messages}
	if len(tools) > 0 {
		// The wire format wraps each definition in a {type:function} envelope.
		wrapped := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			wrapped = append(wrapped, map[string]any{"type": "function", "function": t})
		}
		payload["tools"] = wrapped
		payload["tool_choice"] = "auto"
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ChatResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(p.Config.BaseURL, "chat/completions"), bytes.NewReader(b))
	if err != nil {
		return ChatResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Config.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client(p.Config).Do(req)
	if err != nil {
		return ChatResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return ChatResult{}, fmt.Errorf("text provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseChatResponse(raw)
}

// parseChatResponse is separated so tool-call decoding is testable without a
// live provider.
func parseChatResponse(raw []byte) (ChatResult, error) {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ChatResult{}, fmt.Errorf("text provider returned invalid JSON: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return ChatResult{}, errors.New("text provider returned no choices")
	}
	choice := decoded.Choices[0]
	result := ChatResult{Content: strings.TrimSpace(choice.Message.Content), Finish: choice.FinishReason, Usage: decoded.Usage}
	for _, call := range choice.Message.ToolCalls {
		if strings.TrimSpace(call.Function.Name) == "" {
			return ChatResult{}, errors.New("text provider returned a tool call without a name")
		}
		kind := call.Type
		if kind == "" {
			kind = "function"
		}
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:       call.ID,
			Type:     kind,
			Function: ToolCallFunc{Name: call.Function.Name, Arguments: call.Function.Arguments},
		})
	}
	if result.Content == "" && len(result.ToolCalls) == 0 {
		return ChatResult{}, errors.New("text provider returned empty response")
	}
	return result, nil
}

// AssistantMessage rebuilds the assistant turn so it can be replayed into the
// next request. Providers reject a tool result whose originating call is absent.
func (r ChatResult) AssistantMessage() Message {
	return Message{Role: "assistant", Content: r.Content, ToolCalls: r.ToolCalls}
}

// ToolMessage wraps one tool result for the follow-up request.
func ToolMessage(callID, name, content string) Message {
	return domain.ToolMessage(callID, name, content)
}
