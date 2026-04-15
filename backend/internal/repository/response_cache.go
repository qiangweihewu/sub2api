package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

type responseCacheStore struct {
	rdb *redis.Client
}

// NewResponseCacheStore creates a Redis-backed ResponseCacheStore.
func NewResponseCacheStore(rdb *redis.Client) service.ResponseCacheStore {
	return &responseCacheStore{rdb: rdb}
}

func (s *responseCacheStore) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := s.rdb.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	return val, nil
}

func (s *responseCacheStore) Set(ctx context.Context, key string, data []byte, ttl time.Duration) error {
	return s.rdb.Set(ctx, key, data, ttl).Err()
}
