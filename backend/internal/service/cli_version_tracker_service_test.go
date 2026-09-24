package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

// fakeSettingRepo 内存版 SettingRepository，仅实现 tracker 用到的方法。
type fakeSettingRepo struct {
	store map[string]string
}

func newFakeSettingRepo(initial map[string]string) *fakeSettingRepo {
	store := map[string]string{}
	for k, v := range initial {
		store[k] = v
	}
	return &fakeSettingRepo{store: store}
}

func (f *fakeSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	if v, ok := f.store[key]; ok {
		return &Setting{Key: key, Value: v}, nil
	}
	return nil, nil
}
func (f *fakeSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	return f.store[key], nil
}
func (f *fakeSettingRepo) Set(ctx context.Context, key, value string) error {
	f.store[key] = value
	return nil
}
func (f *fakeSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := f.store[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (f *fakeSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		f.store[k] = v
	}
	return nil
}
func (f *fakeSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range f.store {
		out[k] = v
	}
	return out, nil
}
func (f *fakeSettingRepo) Delete(ctx context.Context, key string) error {
	delete(f.store, key)
	return nil
}

func TestPushVersionFront_DedupsAndCaps(t *testing.T) {
	got := pushVersionFront([]string{"2.1.116", "2.1.115"}, "2.1.117", 3)
	require.Equal(t, []string{"2.1.117", "2.1.116", "2.1.115"}, got)

	// 已存在则去重并提到头部
	got = pushVersionFront([]string{"2.1.117", "2.1.116", "2.1.115"}, "2.1.116", 3)
	require.Equal(t, []string{"2.1.116", "2.1.117", "2.1.115"}, got)

	// 截断到 max
	got = pushVersionFront([]string{"2.1.116", "2.1.115", "2.1.114"}, "2.1.117", 3)
	require.Equal(t, []string{"2.1.117", "2.1.116", "2.1.115"}, got)
}

func TestReloadFromDB_AppliesVersion(t *testing.T) {
	orig := claude.GetCLICurrentVersion()
	origRecent := GetCachedRecentVersions()
	t.Cleanup(func() {
		claude.SetCLICurrentVersion(orig)
		SetCachedRecentVersions(origRecent)
	})

	repo := newFakeSettingRepo(map[string]string{
		SettingKeyCLICurrentVersion: "2.1.300",
		SettingKeyCLIRecentVersions: `["2.1.300","2.1.299"]`,
	})
	svc := NewCLIVersionTrackerService(repo, config.CLIVersionTrackerConfig{
		Enabled:           false, // 不启动 ticker，仅测试 reload
		MaxRecentVersions: 3,
	})
	require.NoError(t, svc.ReloadFromDB(context.Background()))
	require.Equal(t, "2.1.300", claude.GetCLICurrentVersion())
	require.Equal(t, []string{"2.1.300", "2.1.299"}, GetCachedRecentVersions())
}

func TestReloadFromDB_InvalidVersionIgnored(t *testing.T) {
	orig := claude.GetCLICurrentVersion()
	t.Cleanup(func() { claude.SetCLICurrentVersion(orig) })

	repo := newFakeSettingRepo(map[string]string{
		SettingKeyCLICurrentVersion: "not-a-version",
	})
	svc := NewCLIVersionTrackerService(repo, config.CLIVersionTrackerConfig{Enabled: false})
	require.NoError(t, svc.ReloadFromDB(context.Background()))
	require.Equal(t, orig, claude.GetCLICurrentVersion(), "invalid version should be ignored")
}

func TestRunOnce_FetchAndPersist(t *testing.T) {
	orig := claude.GetCLICurrentVersion()
	origRecent := GetCachedRecentVersions()
	t.Cleanup(func() {
		claude.SetCLICurrentVersion(orig)
		SetCachedRecentVersions(origRecent)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"latest": "2.1.300", "beta": "2.2.0-beta.1"})
	}))
	t.Cleanup(server.Close)

	repo := newFakeSettingRepo(map[string]string{
		SettingKeyCLICurrentVersion: "2.1.299",
		SettingKeyCLIRecentVersions: `["2.1.299","2.1.298"]`,
	})
	// 先把全局 var 调到 2.1.299，让 runOnce 检测到 latest=2.1.300 是升级
	claude.SetCLICurrentVersion("2.1.299")

	svc := NewCLIVersionTrackerService(repo, config.CLIVersionTrackerConfig{
		Enabled:           false,
		NpmRegistryURL:    server.URL,
		RequestTimeoutSec: 5,
		MaxRecentVersions: 3,
	})
	svc.runOnce(context.Background())

	require.Equal(t, "2.1.300", claude.GetCLICurrentVersion())
	require.Equal(t, "2.1.300", repo.store[SettingKeyCLICurrentVersion])
	var recent []string
	require.NoError(t, json.Unmarshal([]byte(repo.store[SettingKeyCLIRecentVersions]), &recent))
	require.Equal(t, []string{"2.1.300", "2.1.299", "2.1.298"}, recent)
	require.Equal(t, recent, GetCachedRecentVersions())
}

func TestRunOnce_NpmFailureNoOp(t *testing.T) {
	orig := claude.GetCLICurrentVersion()
	t.Cleanup(func() { claude.SetCLICurrentVersion(orig) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	repo := newFakeSettingRepo(nil)
	svc := NewCLIVersionTrackerService(repo, config.CLIVersionTrackerConfig{
		Enabled:           false,
		NpmRegistryURL:    server.URL,
		RequestTimeoutSec: 2,
	})
	svc.runOnce(context.Background())
	require.Empty(t, repo.store[SettingKeyCLICurrentVersion], "must not write on fetch failure")
}

func TestRunOnce_SameVersionNoWrite(t *testing.T) {
	orig := claude.GetCLICurrentVersion()
	t.Cleanup(func() { claude.SetCLICurrentVersion(orig) })

	claude.SetCLICurrentVersion("2.1.500")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"latest": "2.1.500"})
	}))
	t.Cleanup(server.Close)

	repo := newFakeSettingRepo(nil)
	svc := NewCLIVersionTrackerService(repo, config.CLIVersionTrackerConfig{
		Enabled:        false,
		NpmRegistryURL: server.URL,
	})
	svc.runOnce(context.Background())
	require.Empty(t, repo.store[SettingKeyCLICurrentVersion], "no-op when version unchanged")
}

func TestPickCLIVersionForAccount_UsesCachedRecent(t *testing.T) {
	origRecent := GetCachedRecentVersions()
	t.Cleanup(func() { SetCachedRecentVersions(origRecent) })

	SetCachedRecentVersions([]string{"9.9.9"})
	got := PickCLIVersionForAccount(42)
	require.Equal(t, "9.9.9", got)
}
