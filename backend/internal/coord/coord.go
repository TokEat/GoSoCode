// Package coord provides the distributed-coordination primitives that let
// multiple backend instances safely share the same Baidu account:
//
//   - Locker: a distributed mutex (e.g. serialize token refresh, or serialize
//     create() on the same remote path across instances).
//   - Limiter: a per-key rate limiter to keep each account's QPS under Baidu's
//     risk-control threshold cluster-wide.
//
// Two backends are provided: an in-memory one (single instance / dev) and a
// Redis one (multi-instance / prod). Selection is by config.
package coord

import (
	"context"
	"time"
)

// Locker is a distributed mutex. Lock blocks until the lock is held or ctx is
// done; the returned unlock func must be called to release it.
type Locker interface {
	Lock(ctx context.Context, key string, ttl time.Duration) (unlock func(), err error)
}

// Limiter throttles operations per key to a configured rate.
type Limiter interface {
	Wait(ctx context.Context, key string) error
}

// Coordinator bundles both primitives.
type Coordinator struct {
	Locker  Locker
	Limiter Limiter
}

// New returns a Redis-backed coordinator when redisAddr is set, otherwise an
// in-memory one. qps is the per-key rate limit for the Limiter.
func New(redisAddr string, qps int) (*Coordinator, error) {
	if redisAddr == "" {
		return &Coordinator{
			Locker:  newMemLocker(),
			Limiter: newMemLimiter(qps),
		}, nil
	}
	return newRedisCoordinator(redisAddr, qps)
}
