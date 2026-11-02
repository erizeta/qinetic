package qinetic

import (
	"context"
	"time"
)

// Storage is a key-value store with expiry, used by middleware such as limiter, csrf, and cache.
// Implementations must be safe for concurrent use. The default, used when a middleware is given none,
// keeps entries in the process's memory, so state is not shared between instances; plug in an external
// store, such as Redis, to share it.
type Storage interface {
	// Get returns the value of key, or nil and no error when the key is missing or expired.
	Get(ctx context.Context, key string) ([]byte, error)
	// Set stores val under key. A ttl of zero or less means the entry does not expire.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Close releases the storage's resources.
	Close() error
}

// Incrementer is implemented by a Storage with atomic counters, such as Redis INCRBY. Middleware use it
// when available, so counts are exact even across instances sharing the storage.
type Incrementer interface {
	// Incr adds delta, which may be negative, to the integer at key and returns the new value and when the
	// key expires. A missing or expired key starts at zero and gets ttl; an existing key keeps its expiry.
	// Get on the key returns the value in decimal.
	Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, time.Time, error)
}
