// Package cache connects to Redis.
//
// Redis is a very fast in-memory store. We use it for:
//   - caching sessions for a minute (so every request doesn't hit MySQL)
//   - short-lived locks ("only one scheduler runs the rate collector")
//   - rate limits and the idempotency "double-tap" guard
//   - publishing live rate updates to every API server (pub/sub)
//   - the job queue (asynq stores jobs in Redis)
//
// Redis is NEVER the source of truth for money. MySQL is.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tribe-africa/tribe-backend/config"
)

// Connect opens the Redis connection and checks it works.
func Connect(settings config.RedisConfig) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     settings.Host + ":" + settings.Port,
		Password: settings.Password,
	})
	pingContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(pingContext).Err(); err != nil {
		return nil, fmt.Errorf("cannot reach Redis at %s:%s: %w", settings.Host, settings.Port, err)
	}
	return client, nil
}

// Lock is a simple Redis lock: SET key value NX PX duration.
// "NX" means "only if the key does not exist yet", so only one caller wins.
type Lock struct {
	client *redis.Client
	key    string
	owner  string
}

// TryLock tries to take a lock for the given time. It returns nil if
// someone else holds it.
func TryLock(requestContext context.Context, client *redis.Client, key string, owner string, duration time.Duration) (*Lock, error) {
	taken, err := client.SetNX(requestContext, key, owner, duration).Result()
	if err != nil || !taken {
		return nil, err
	}
	return &Lock{client: client, key: key, owner: owner}, nil
}

// releaseScript deletes the key only if we still own it (so we never
// release a lock that expired and was taken by someone else).
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// extendScript pushes the expiry out, again only if we still own it.
var extendScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0`)

// Release gives the lock back.
func (lock *Lock) Release(requestContext context.Context) {
	releaseScript.Run(requestContext, lock.client, []string{lock.key}, lock.owner)
}

// Extend keeps holding the lock for longer. It returns false if we lost it.
func (lock *Lock) Extend(requestContext context.Context, duration time.Duration) bool {
	result, err := extendScript.Run(requestContext, lock.client, []string{lock.key}, lock.owner, duration.Milliseconds()).Int()
	return err == nil && result == 1
}

// takeAttemptScript adds one to a counter and starts its expiry clock on the
// first attempt, in ONE atomic step. Doing INCR and EXPIRE as two separate
// calls could leave a counter that never expires if the program stopped in
// between.
var takeAttemptScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
	redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return count`)

// TakeAttempt counts one attempt (a sign-in, a password check) against key
// and returns false once more than `limit` attempts were made inside
// `window`. The check and the count are the SAME atomic step, so 50
// parallel requests get 50 different counts and only the first `limit` go
// through. Checking first and counting later, as two steps, would let a
// burst of parallel requests all pass the check before any of them counted.
//
// Call it BEFORE the expensive or sensitive work (hashing a password), and
// call ForgetAttempts after a success so a real user starts again from zero.
//
// If Redis cannot be reached the attempt is allowed ("fail open"), so a
// Redis outage does not lock every customer out. The hashing slots in
// security.Toolkit still protect the server's memory in that case.
func TakeAttempt(requestContext context.Context, client *redis.Client, key string, limit int, window time.Duration) bool {
	count, err := takeAttemptScript.Run(requestContext, client, []string{key}, window.Milliseconds()).Int64()
	if err != nil {
		return true
	}
	return count <= int64(limit)
}

// ForgetAttempts clears a TakeAttempt counter after a successful check.
func ForgetAttempts(requestContext context.Context, client *redis.Client, key string) {
	client.Del(requestContext, key)
}
