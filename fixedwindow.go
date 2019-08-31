package limiters

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
)

// FixedWindowIncrementer wraps the Increment method.
type FixedWindowIncrementer interface {
	// Increment increments the request counter for the window.
	// TTL provides the time duration before the window is changed.
	Increment(ctx context.Context, window time.Time, ttl time.Duration) (int64, error)
}

// FixedWindow implements a Fixed Window rate limiting algorithm.
type FixedWindow struct {
	FixedWindowIncrementer
	Clock
	Rate     time.Duration
	Capacity int64
}

// NewFixedWindow creates a new instance of FixedWindow.
// Capacity is the maximum amount of requests allowed per window.
// Rate is the window size.
func NewFixedWindow(capacity int64, rate time.Duration, fixedWindowIncrementer FixedWindowIncrementer, clock Clock) *FixedWindow {
	return &FixedWindow{FixedWindowIncrementer: fixedWindowIncrementer, Clock: clock, Rate: rate, Capacity: capacity}
}

// Limit returns the time duration to wait before the request can be processed.
// It returns ErrLimitExhausted if the request overflows the window's capacity.
func (f *FixedWindow) Limit(ctx context.Context) (time.Duration, error) {
	now := f.Now()
	window := now.Truncate(f.Rate)
	ttl := f.Rate - now.Sub(window)
	c, err := f.Increment(ctx, window, ttl)
	if err != nil {
		return 0, err
	}
	if c > f.Capacity {
		return ttl, ErrLimitExhausted
	}
	return 0, nil
}

// FixedWindowInMemory is an in-memory implementation of FixedWindow.
type FixedWindowInMemory struct {
	mu     sync.Mutex
	c      int64
	window time.Time
}

// NewFixedWindowInMemory creates a new instance of FixedWindowInMemory.
func NewFixedWindowInMemory() *FixedWindowInMemory {
	return &FixedWindowInMemory{}
}

// Increment increments the window's counter.
func (f *FixedWindowInMemory) Increment(ctx context.Context, window time.Time, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if window != f.window {
		f.c = 0
		f.window = window
	}
	f.c++
	return f.c, ctx.Err()
}

// FixedWindowRedis implements FixedWindow in Redis.
type FixedWindowRedis struct {
	cli    *redis.Client
	prefix string
}

// NewFixedWindowRedis returns a new instance of FixedWindowRedis.
// Prefix is the key prefix used to store all the keys used in this implementation in Redis.
func NewFixedWindowRedis(cli *redis.Client, prefix string) *FixedWindowRedis {
	return &FixedWindowRedis{cli: cli, prefix: prefix}
}

// Increment increments the window's counter in Redis.
func (f *FixedWindowRedis) Increment(ctx context.Context, window time.Time, ttl time.Duration) (int64, error) {
	var incr *redis.IntCmd
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err = f.cli.Pipelined(func(pipeliner redis.Pipeliner) error {
			key := fmt.Sprintf("%d", window.UnixNano())
			incr = pipeliner.Incr(redisKey(f.prefix, key))
			pipeliner.PExpire(redisKey(f.prefix, key), ttl)
			return nil
		})
	}()

	select {
	case <-done:
		if err != nil {
			return 0, errors.Wrap(err, "redis transaction failed")
		}
		return incr.Val(), incr.Err()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}