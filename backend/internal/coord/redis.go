package coord

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// newRedisCoordinator wires Redis-backed lock + limiter for multi-instance
// deployments so all backend replicas coordinate on the same shared account.
func newRedisCoordinator(addr string, qps int) (*Coordinator, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Coordinator{
		Locker:  &redisLocker{rdb: rdb},
		Limiter: &redisLimiter{rdb: rdb, qps: qps},
	}, nil
}

// ---- Redis Locker: SET NX PX with a fencing token --------------------------

type redisLocker struct{ rdb *redis.Client }

// releaseScript deletes the key only if we still own it (value matches token),
// preventing release of a lock that already expired and was re-acquired.
var releaseScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
  return redis.call("del", KEYS[1])
end
return 0`)

func (r *redisLocker) Lock(ctx context.Context, key string, ttl time.Duration) (func(), error) {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	token := uuid.NewString()
	rkey := "lock:" + key
	backoff := 20 * time.Millisecond
	for {
		ok, err := r.rdb.SetNX(ctx, rkey, token, ttl).Result()
		if err != nil {
			return nil, err
		}
		if ok {
			unlock := func() {
				bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = releaseScript.Run(bg, r.rdb, []string{rkey}, token).Err()
			}
			return unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
			if backoff < 500*time.Millisecond {
				backoff *= 2
			}
		}
	}
}

// ---- Redis Limiter: token bucket in a Lua script ---------------------------

type redisLimiter struct {
	rdb *redis.Client
	qps int
}

// bucketScript implements a token bucket: refills at `rate` tokens/sec up to
// `burst`, returns wait-millis until a token is available (0 if immediate).
var bucketScript = redis.NewScript(`
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local state = redis.call("hmget", key, "tokens", "ts")
local tokens = tonumber(state[1])
local ts = tonumber(state[2])
if tokens == nil then tokens = burst; ts = now end
local delta = math.max(0, now - ts) / 1000.0
tokens = math.min(burst, tokens + delta * rate)
local wait = 0
if tokens >= 1 then
  tokens = tokens - 1
else
  wait = math.ceil((1 - tokens) / rate * 1000)
end
redis.call("hmset", key, "tokens", tokens, "ts", now)
redis.call("pexpire", key, 60000)
return wait`)

func (r *redisLimiter) Wait(ctx context.Context, key string) error {
	qps := r.qps
	if qps <= 0 {
		qps = 8
	}
	rkey := "rl:" + key
	for {
		now := time.Now().UnixMilli()
		res, err := bucketScript.Run(ctx, r.rdb, []string{rkey}, qps, qps, now).Int64()
		if err != nil {
			return err
		}
		if res <= 0 {
			return nil
		}
		wait := time.Duration(res) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
