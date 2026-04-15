package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/tidwall/gjson"
)

// ResponseCacheStore defines the cache storage interface.
type ResponseCacheStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, data []byte, ttl time.Duration) error
}

// ResponseCacheService provides response-level exact-match caching.
// When temperature=0 (deterministic), identical requests produce identical outputs,
// so we can cache and replay the full HTTP response (including SSE streams).
type ResponseCacheService struct {
	store      ResponseCacheStore
	ttl        time.Duration
	maxSizeBytes int
}

// NewResponseCacheService creates a new ResponseCacheService.
func NewResponseCacheService(store ResponseCacheStore, ttlMinutes, maxSizeMB int) *ResponseCacheService {
	if ttlMinutes <= 0 {
		ttlMinutes = 120 // default 2 hours
	}
	if maxSizeMB <= 0 {
		maxSizeMB = 10 // default 10MB
	}
	return &ResponseCacheService{
		store:        store,
		ttl:          time.Duration(ttlMinutes) * time.Minute,
		maxSizeBytes: maxSizeMB * 1024 * 1024,
	}
}

// IsCacheable returns true if the request body is eligible for response caching.
// Only deterministic requests (temperature=0 or absent, top_p=1 or absent) are cached.
func IsCacheable(body []byte) bool {
	temp := gjson.GetBytes(body, "temperature")
	if temp.Exists() && temp.Float() > 0 {
		return false
	}
	topP := gjson.GetBytes(body, "top_p")
	if topP.Exists() && topP.Float() < 1.0 {
		return false
	}
	return true
}

// ComputeCacheKey generates a SHA256 cache key from the request fields that affect
// the response content. Fields that do NOT affect the output (stream, metadata,
// cache_control markers) are excluded.
func ComputeCacheKey(groupID *int64, body []byte) string {
	model := gjson.GetBytes(body, "model").String()
	system := gjson.GetBytes(body, "system").Raw
	messages := gjson.GetBytes(body, "messages").Raw
	tools := gjson.GetBytes(body, "tools").Raw
	maxTokens := gjson.GetBytes(body, "max_tokens").Raw
	thinking := gjson.GetBytes(body, "thinking").Raw

	h := sha256.New()
	gid := int64(0)
	if groupID != nil {
		gid = *groupID
	}
	fmt.Fprintf(h, "%d|%s|%s|%s|%s|%s|%s", gid, model, system, messages, tools, maxTokens, thinking)
	return "rc:" + hex.EncodeToString(h.Sum(nil))
}

// Get retrieves a cached response. Returns nil, false on miss.
func (s *ResponseCacheService) Get(ctx context.Context, key string) ([]byte, bool) {
	data, err := s.store.Get(ctx, key)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// Set stores a response in cache. Responses exceeding maxSizeBytes are not cached.
func (s *ResponseCacheService) Set(ctx context.Context, key string, data []byte) {
	if len(data) == 0 || len(data) > s.maxSizeBytes {
		return
	}
	_ = s.store.Set(ctx, key, data, s.ttl)
}

// ProvideResponseCacheService creates a ResponseCacheService if enabled in config,
// otherwise returns nil (cache is a no-op).
func ProvideResponseCacheService(store ResponseCacheStore, cfg *config.Config) *ResponseCacheService {
	if cfg == nil || !cfg.Gateway.ResponseCache.Enabled {
		return nil
	}
	return NewResponseCacheService(store, cfg.Gateway.ResponseCache.TTLMinutes, cfg.Gateway.ResponseCache.MaxSizeMB)
}
