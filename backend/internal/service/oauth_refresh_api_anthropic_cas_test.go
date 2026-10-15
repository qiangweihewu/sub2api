//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// anthropicCASRefreshRepo adds the refresh_token-guarded patch write to the
// shared refresh API repo stub.
type anthropicCASRefreshRepo struct {
	*refreshAPIAccountRepo
	patchCalls        int
	lastExpectedToken string
	lastPatch         map[string]any
}

func (r *anthropicCASRefreshRepo) PatchOAuthCredentialsIfRefreshTokenUnchanged(
	_ context.Context,
	id int64,
	expectedRefreshToken string,
	patch map[string]any,
) (bool, error) {
	r.patchCalls++
	r.lastExpectedToken = expectedRefreshToken
	r.lastPatch = shallowCopyMap(patch)
	if r.updateErr != nil {
		return false, r.updateErr
	}
	if r.account == nil || r.account.ID != id || r.account.GetCredential("refresh_token") != expectedRefreshToken {
		return false, nil
	}
	merged := shallowCopyMap(r.account.Credentials)
	for key, value := range patch {
		merged[key] = value
	}
	r.account.Credentials = merged
	return true, nil
}

func newAnthropicCASRefreshTestAccount() *Account {
	return &Account{
		ID:       77,
		Platform: PlatformAnthropic,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":  "old-session-access",
			"refresh_token": "old-session-refresh",
			"model_mapping": map[string]any{"a": "b"},
		},
	}
}

func refreshResultFor(account *Account, accessToken, refreshToken string) map[string]any {
	creds := shallowCopyMap(account.Credentials)
	creds["access_token"] = accessToken
	creds["refresh_token"] = refreshToken
	return creds
}

// 在途刷新期间管理员完成了重新授权：旧会话的刷新结果不得覆盖新授权。
func TestOAuthRefreshAPI_AnthropicRefreshDoesNotOverwriteReauthorization(t *testing.T) {
	account := newAnthropicCASRefreshTestAccount()
	repo := &anthropicCASRefreshRepo{refreshAPIAccountRepo: &refreshAPIAccountRepo{account: account}}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials:  refreshResultFor(account, "old-session-rotated-access", "old-session-rotated-refresh"),
		onRefresh: func() {
			reauthorized := *account
			reauthorized.Credentials = map[string]any{
				"access_token":  "admin-access",
				"refresh_token": "admin-refresh",
				"model_mapping": map[string]any{"a": "b"},
			}
			repo.account = &reauthorized
		},
	}

	result, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.NoError(t, err)
	require.False(t, result.Refreshed)
	require.Equal(t, "admin-refresh", repo.account.GetCredential("refresh_token"))
	require.Equal(t, "admin-access", result.Account.GetCredential("access_token"), "caller must continue with the re-authorized token")
	require.Equal(t, "old-session-refresh", repo.lastExpectedToken)
	require.Zero(t, repo.updateCredentialsCalls, "must not fall back to unconditional write")
}

// setup-token 账号同样经 ClaudeTokenRefresher 刷新（IsOAuth 含 setup-token），必须走同一条件写入。
func TestOAuthRefreshAPI_AnthropicSetupTokenRefreshDoesNotOverwriteReauthorization(t *testing.T) {
	account := newAnthropicCASRefreshTestAccount()
	account.Type = AccountTypeSetupToken
	repo := &anthropicCASRefreshRepo{refreshAPIAccountRepo: &refreshAPIAccountRepo{account: account}}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials:  refreshResultFor(account, "old-session-rotated-access", "old-session-rotated-refresh"),
		onRefresh: func() {
			reauthorized := *account
			reauthorized.Credentials = map[string]any{"access_token": "admin-access", "refresh_token": "admin-refresh"}
			repo.account = &reauthorized
		},
	}

	result, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.NoError(t, err)
	require.False(t, result.Refreshed)
	require.Equal(t, 1, repo.patchCalls)
	require.Equal(t, "admin-refresh", repo.account.GetCredential("refresh_token"))
}

// 正常刷新只补丁刷新改动的字段，不回滚刷新期间并发修改的非认证字段。
func TestOAuthRefreshAPI_AnthropicRefreshPatchesOnlyChangedFields(t *testing.T) {
	account := newAnthropicCASRefreshTestAccount()
	repo := &anthropicCASRefreshRepo{refreshAPIAccountRepo: &refreshAPIAccountRepo{account: account}}
	executor := &refreshAPIExecutorStub{
		needsRefresh: true,
		credentials:  refreshResultFor(account, "rotated-access", "rotated-refresh"),
		onRefresh: func() {
			edited := *account
			edited.Credentials = shallowCopyMap(account.Credentials)
			edited.Credentials["model_mapping"] = map[string]any{"a": "edited"}
			repo.account = &edited
		},
	}

	result, err := NewOAuthRefreshAPI(repo, &refreshAPICacheStub{lockResult: true}).RefreshIfNeeded(context.Background(), account, executor, time.Minute)

	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, 1, repo.patchCalls)
	require.NotContains(t, repo.lastPatch, "model_mapping")
	require.Contains(t, repo.lastPatch, "_token_version")
	require.Equal(t, "rotated-refresh", repo.account.GetCredential("refresh_token"))
	require.Equal(t, map[string]any{"a": "edited"}, repo.account.Credentials["model_mapping"])
	require.Equal(t, "rotated-access", result.Account.GetCredential("access_token"))
}

func TestChangedCredentialFields(t *testing.T) {
	patch := changedCredentialFields(
		map[string]any{"same": "x", "changed": "old", "nested": map[string]any{"k": "v"}},
		map[string]any{"same": "x", "changed": "new", "nested": map[string]any{"k": "v"}, "added": 1},
	)
	require.Equal(t, map[string]any{"changed": "new", "added": 1}, patch)
}
