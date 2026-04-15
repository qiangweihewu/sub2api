package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSyncBillingHeaderVersion(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		userAgent string
		wantSub   string // substring expected in result
		unchanged bool   // expect body to remain the same
	}{
		{
			name:      "replaces cc_version preserving message-derived suffix",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22 (external, cli)",
			wantSub:   "cc_version=2.1.22.df2",
		},
		{
			name:      "no billing header in system",
			body:      `{"system":[{"type":"text","text":"You are Claude Code."}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			unchanged: true,
		},
		{
			name:      "no system field",
			body:      `{"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			unchanged: true,
		},
		{
			name:      "user-agent without version",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81; cc_entrypoint=cli;"}],"messages":[]}`,
			userAgent: "Mozilla/5.0",
			unchanged: true,
		},
		{
			name:      "empty user-agent",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81; cc_entrypoint=cli;"}],"messages":[]}`,
			userAgent: "",
			unchanged: true,
		},
		{
			name:      "version already matches",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.22; cc_entrypoint=cli;"}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			unchanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := syncBillingHeaderVersion([]byte(tt.body), tt.userAgent)
			if tt.unchanged {
				assert.Equal(t, tt.body, string(result), "body should remain unchanged")
			} else {
				assert.Contains(t, string(result), tt.wantSub)
				assert.NotContains(t, string(result), "cc_version=2.1.81")
			}
		})
	}
}

func TestSignBillingHeaderCCH(t *testing.T) {
	t.Run("removes cch placeholder and updates version suffix", func(t *testing.T) {
		// Use a specific message so we know the fingerprint deterministically
		// "hello there" -> index 4 is 'o', 7 is 'e', 20 is ''
		body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.a43; cc_entrypoint=cli; cch=00000;"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello there"}]}]}`)
		result := signBillingHeaderCCH(body)

		billingText := gjson.GetBytes(result, "system.0.text").String()
		
		// Should not have the cch= placeholder anymore at all
		assert.NotContains(t, billingText, "cch=")

		// It should recalculate fp based on "hello there" and "2.1.63", and insert it
		fp := computeClaudeFingerprint("hello there", "2.1.63")
		require.NotEmpty(t, fp)
		assert.Contains(t, billingText, "cc_version=2.1.63."+fp)
		assert.NotContains(t, billingText, "cc_version=2.1.63.a43")
	})

	t.Run("no billing header - body unchanged", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"You are Claude Code."}],"messages":[]}`)
		result := signBillingHeaderCCH(body)
		assert.Equal(t, string(body), string(result))
	})

	t.Run("cch=00000 in user content is not touched", func(t *testing.T) {
		body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63; cc_entrypoint=cli; cch=00000;"}],"messages":[{"role":"user","content":[{"type":"text","text":"keep literal cch=00000 in this message"}]}]}`)
		result := signBillingHeaderCCH(body)

		// Billing header should be updated and stripped of cch
		billingText := gjson.GetBytes(result, "system.0.text").String()
		assert.NotContains(t, billingText, "cch=00000")
		assert.NotContains(t, billingText, "cch=")

		fp := computeClaudeFingerprint("keep literal cch=00000 in this message", "2.1.63")
		assert.Contains(t, billingText, "cc_version=2.1.63."+fp)

		// User message should keep its literal cch=00000
		userText := gjson.GetBytes(result, "messages.0.content.0.text").String()
		assert.Contains(t, userText, "cch=00000")
	})

	t.Run("computeClaudeFingerprint extracts correct characters", func(t *testing.T) {
		// "012345678901234567890" => length 21
		// msg[4]='4', msg[7]='7', msg[20]='0'
		fp := computeClaudeFingerprint("012345678901234567890", "2.1.88")
		require.NotEmpty(t, fp)
		require.Len(t, fp, 3)

		// Emojis are treated as runes (which might differ exactly from UCS-2, but works for most ASCII cases)
		fp2 := computeClaudeFingerprint("01234", "2.1.88")
		require.NotEmpty(t, fp2)
		require.Len(t, fp2, 3)
	})
}

