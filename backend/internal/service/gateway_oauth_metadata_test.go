package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildOAuthMetadataUserID_FallbackWithoutAccountUUID(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
		System:         nil,
		Messages:       nil,
	}

	account := &Account{
		ID:    123,
		Type:  AccountTypeOAuth,
		Extra: map[string]any{}, // intentionally missing account_uuid / claude_user_id
	}

	fp := &Fingerprint{ClientID: "deadbeef"} // should be used as device_id

	got := svc.buildOAuthMetadataUserID(parsed, account, fp)
	require.NotEmpty(t, got)

	// mimic UA >= 2.1.78 → JSON format
	var j struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &j), "should be valid JSON: %s", got)
	require.Equal(t, "deadbeef", j.DeviceID)
	require.Equal(t, "", j.AccountUUID)
	require.NotEmpty(t, j.SessionID)
}

func TestBuildOAuthMetadataUserID_UsesAccountUUIDWhenPresent(t *testing.T) {
	svc := &GatewayService{}

	parsed := &ParsedRequest{
		Model:          "claude-sonnet-4-5",
		Stream:         true,
		MetadataUserID: "",
	}

	account := &Account{
		ID:   123,
		Type: AccountTypeOAuth,
		Extra: map[string]any{
			"account_uuid":      "acc-uuid",
			"claude_user_id":    "clientid123",
			"anthropic_user_id": "",
		},
	}

	got := svc.buildOAuthMetadataUserID(parsed, account, nil)
	require.NotEmpty(t, got)

	// mimic UA >= 2.1.78 → JSON format
	var j struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(got), &j), "should be valid JSON: %s", got)
	require.Equal(t, "clientid123", j.DeviceID)
	require.Equal(t, "acc-uuid", j.AccountUUID)
	require.NotEmpty(t, j.SessionID)
}
