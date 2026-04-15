package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- IsCacheable ---

func TestIsCacheable_Temperature0(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	assert.True(t, IsCacheable(body), "no temperature = cacheable")
}

func TestIsCacheable_TemperatureExplicit0(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","temperature":0,"messages":[{"role":"user","content":"hello"}]}`)
	assert.True(t, IsCacheable(body), "temperature=0 = cacheable")
}

func TestIsCacheable_TemperatureNonZero(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","temperature":0.7,"messages":[{"role":"user","content":"hello"}]}`)
	assert.False(t, IsCacheable(body), "temperature=0.7 = not cacheable")
}

func TestIsCacheable_TopPLessThan1(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","top_p":0.9,"messages":[{"role":"user","content":"hello"}]}`)
	assert.False(t, IsCacheable(body), "top_p=0.9 = not cacheable")
}

// --- ComputeCacheKey ---

func TestComputeCacheKey_DeterministicForSameInput(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"system":"be helpful"}`)
	gid := int64(1)
	k1 := ComputeCacheKey(&gid, body)
	k2 := ComputeCacheKey(&gid, body)
	assert.Equal(t, k1, k2, "same input produces same key")
	assert.True(t, len(k1) > 10, "key should be non-trivial")
}

func TestComputeCacheKey_DifferentGroupID(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	gid1 := int64(1)
	gid2 := int64(2)
	k1 := ComputeCacheKey(&gid1, body)
	k2 := ComputeCacheKey(&gid2, body)
	assert.NotEqual(t, k1, k2, "different group_id produces different key")
}

func TestComputeCacheKey_MetadataDoesNotAffectKey(t *testing.T) {
	body1 := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user1"}}`)
	body2 := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"user2"}}`)
	gid := int64(1)
	k1 := ComputeCacheKey(&gid, body1)
	k2 := ComputeCacheKey(&gid, body2)
	assert.Equal(t, k1, k2, "metadata should not affect cache key")
}

func TestComputeCacheKey_StreamDoesNotAffectKey(t *testing.T) {
	body1 := []byte(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	body2 := []byte(`{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"hello"}]}`)
	gid := int64(1)
	k1 := ComputeCacheKey(&gid, body1)
	k2 := ComputeCacheKey(&gid, body2)
	assert.Equal(t, k1, k2, "stream flag should not affect cache key")
}

func TestComputeCacheKey_DifferentMessagesDifferentKey(t *testing.T) {
	body1 := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	body2 := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"goodbye"}]}`)
	gid := int64(1)
	k1 := ComputeCacheKey(&gid, body1)
	k2 := ComputeCacheKey(&gid, body2)
	assert.NotEqual(t, k1, k2, "different messages should produce different key")
}

func TestComputeCacheKey_NilGroupID(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	k := ComputeCacheKey(nil, body)
	require.NotEmpty(t, k)
	assert.True(t, len(k) > 3, "should produce valid key even with nil group")
}

// --- ResponseCacheService Get/Set ---

type mockCacheStore struct {
	data map[string][]byte
}

func newMockCacheStore() *mockCacheStore {
	return &mockCacheStore{data: make(map[string][]byte)}
}

func (m *mockCacheStore) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, nil
	}
	return v, nil
}

func (m *mockCacheStore) Set(_ context.Context, key string, data []byte, _ time.Duration) error {
	m.data[key] = data
	return nil
}

func TestResponseCacheService_GetSet(t *testing.T) {
	store := newMockCacheStore()
	svc := NewResponseCacheService(store, 60, 10)

	ctx := context.Background()

	// Miss
	_, ok := svc.Get(ctx, "key1")
	assert.False(t, ok)

	// Set
	svc.Set(ctx, "key1", []byte("hello"))

	// Hit
	data, ok := svc.Get(ctx, "key1")
	assert.True(t, ok)
	assert.Equal(t, []byte("hello"), data)
}

func TestResponseCacheService_SkipsOversized(t *testing.T) {
	store := newMockCacheStore()
	svc := NewResponseCacheService(store, 60, 1) // 1MB max

	ctx := context.Background()
	bigData := make([]byte, 2*1024*1024) // 2MB
	svc.Set(ctx, "big", bigData)

	_, ok := svc.Get(ctx, "big")
	assert.False(t, ok, "oversized response should not be cached")
}
