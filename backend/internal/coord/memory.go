package coord

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ---- in-memory Locker: keyed mutexes within one process --------------------

type memLocker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newMemLocker() *memLocker {
	return &memLocker{locks: make(map[string]*sync.Mutex)}
}

func (m *memLocker) keyMutex(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.locks[key]
	if !ok {
		l = &sync.Mutex{}
		m.locks[key] = l
	}
	return l
}

func (m *memLocker) Lock(ctx context.Context, key string, _ time.Duration) (func(), error) {
	km := m.keyMutex(key)
	// Respect ctx cancellation while waiting for the mutex.
	acquired := make(chan struct{})
	go func() { km.Lock(); close(acquired) }()
	select {
	case <-acquired:
		return func() { km.Unlock() }, nil
	case <-ctx.Done():
		// Unlock once the background goroutine finally acquires, to avoid leak.
		go func() { <-acquired; km.Unlock() }()
		return nil, ctx.Err()
	}
}

// ---- in-memory Limiter: token bucket per key -------------------------------

type memLimiter struct {
	mu    sync.Mutex
	qps   int
	byKey map[string]*rate.Limiter
}

func newMemLimiter(qps int) *memLimiter {
	if qps <= 0 {
		qps = 8
	}
	return &memLimiter{qps: qps, byKey: make(map[string]*rate.Limiter)}
}

func (m *memLimiter) Wait(ctx context.Context, key string) error {
	m.mu.Lock()
	l, ok := m.byKey[key]
	if !ok {
		l = rate.NewLimiter(rate.Limit(m.qps), m.qps)
		m.byKey[key] = l
	}
	m.mu.Unlock()
	return l.Wait(ctx)
}
