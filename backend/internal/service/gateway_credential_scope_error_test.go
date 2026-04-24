package service

import "testing"

func TestIsClaudeCodeCredentialScopeError(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want bool
	}{
		{
			name: "legacy credential scope error",
			msg:  "This credential is only authorized for use with Claude Code and cannot be used for other API requests.",
			want: true,
		},
		{
			name: "current third-party detection (2026)",
			msg:  "Third-party apps now draw from your extra usage, not your plan limits. Add more at claude.ai/settings/usage and keep going.",
			want: true,
		},
		{
			name: "third-party with different punctuation",
			msg:  "Third-party apps now draw from your extra usage.",
			want: true,
		},
		{
			name: "unrelated 400 error",
			msg:  "invalid model",
			want: false,
		},
		{
			name: "empty",
			msg:  "",
			want: false,
		},
		{
			name: "metadata extra inputs error (unrelated)",
			msg:  "metadata: Extra inputs are not permitted",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClaudeCodeCredentialScopeError(tt.msg); got != tt.want {
				t.Errorf("isClaudeCodeCredentialScopeError(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
}
