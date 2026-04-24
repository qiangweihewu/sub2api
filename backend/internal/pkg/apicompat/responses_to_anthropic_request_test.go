package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestResponsesToAnthropicRequest_TrailingAssistantAppendsContinue verifies
// that when the converted messages end with an assistant role, a neutral
// "continue" user message is appended. This prevents upstream 400 errors
// from models that reject assistant prefill (e.g. claude-opus-4-6).
func TestResponsesToAnthropicRequest_TrailingAssistantAppendsContinue(t *testing.T) {
	input := []ResponsesInputItem{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"I'll start with:"`)},
	}
	inputJSON, _ := json.Marshal(input)

	req := &ResponsesRequest{
		Model: "claude-opus-4-6",
		Input: inputJSON,
	}

	got, err := ResponsesToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if n := len(got.Messages); n == 0 {
		t.Fatalf("expected messages, got none")
	}
	last := got.Messages[len(got.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("expected last message role=user, got %q", last.Role)
	}

	if !strings.Contains(string(last.Content), "continue") {
		t.Errorf("expected continuation content to contain 'continue', got %s", string(last.Content))
	}

	// The original assistant message must still be present as context.
	hasAssistant := false
	for _, m := range got.Messages {
		if m.Role == "assistant" {
			hasAssistant = true
			break
		}
	}
	if !hasAssistant {
		t.Errorf("expected assistant message preserved in history, not dropped")
	}
}

// TestResponsesToAnthropicRequest_NoTrailingAssistantNoOp verifies that
// when the conversation already ends with a user message, no synthetic
// continuation is added.
func TestResponsesToAnthropicRequest_NoTrailingAssistantNoOp(t *testing.T) {
	input := []ResponsesInputItem{
		{Role: "user", Content: json.RawMessage(`"hi"`)},
		{Role: "assistant", Content: json.RawMessage(`"hello"`)},
		{Role: "user", Content: json.RawMessage(`"how are you?"`)},
	}
	inputJSON, _ := json.Marshal(input)

	req := &ResponsesRequest{
		Model: "claude-opus-4-6",
		Input: inputJSON,
	}

	got, err := ResponsesToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Messages[len(got.Messages)-1].Role != "user" {
		t.Fatalf("last message should be user")
	}

	userCount := 0
	for _, m := range got.Messages {
		if m.Role == "user" {
			userCount++
		}
	}
	if userCount != 2 {
		t.Errorf("expected exactly 2 user messages (no synthetic continue), got %d", userCount)
	}
}
