// Package limiter limits how many requests a client makes per time window.
package limiter

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
)

// Algorithm selects how requests are counted.
type Algorithm int

const (
	// FixedWindow counts requests from a client's first request until Expiration has passed.
	FixedWindow Algorithm = iota
	// SlidingWindow weighs the previous window's count by how much of it still overlaps the last
	// Expiration, so a burst at a window boundary cannot double the rate.
	SlidingWindow
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// Max is the number of requests allowed per window. If zero, 5.
	Max int
	// Expiration is the window length. If zero, one minute.
	Expiration time.Duration
	// KeyGenerator returns the key requests are counted under. If nil, c.IP().
	KeyGenerator func(c *qinetic.Ctx) string
	// Algorithm is FixedWindow (the default) or SlidingWindow.
	Algorithm Algorithm
	// LimitReached responds to a request over the limit. If nil, it returns qinetic.ErrTooManyRequests.
	LimitReached qinetic.Handler
	// SkipFailedRequests and SkipSuccessfulRequests stop counting requests whose status is 400 or more,
	// or under 400, once they are done.
	SkipFailedRequests     bool
	SkipSuccessfulRequests bool
	// Storage holds the counters. If nil, an in-memory storage per middleware is used. A Storage that
	// implements qinetic.Incrementer counts atomically, also across instances; otherwise counts are
	// read and written under a lock, which is exact within one process only.
	Storage qinetic.Storage
}

// now is the clock, replaced in tests.
var now = time.Now

const keyPrefix = "limiter:"

// New returns the middleware. Responses carry X-RateLimit-Limit, X-RateLimit-Remaining, and
// X-RateLimit-Reset (seconds until the current window ends); a request over the limit gets 429 with
// Retry-After. Rejected requests do not count against the limit.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.Max <= 0 {
		cfg.Max = 5
	}
	if cfg.Expiration <= 0 {
		cfg.Expiration = time.Minute
	}
	if cfg.KeyGenerator == nil {
		cfg.KeyGenerator = func(c *qinetic.Ctx) string { return c.IP() }
	}
	if cfg.LimitReached == nil {
		cfg.LimitReached = func(*qinetic.Ctx) error { return qinetic.ErrTooManyRequests }
	}
	if cfg.Storage == nil {
		cfg.Storage = memory.New(memory.Config{Clock: func() time.Time { return now() }})
	}
	var ctr counter = &lockedCounter{s: cfg.Storage}
	if inc, ok := cfg.Storage.(qinetic.Incrementer); ok {
		ctr = atomicCounter{s: cfg.Storage, inc: inc}
	}
	l := &limiter{cfg: cfg, ctr: ctr}
	return l.handle
}

type limiter struct {
	cfg Config
	ctr counter
}

// window is the outcome of counting one request.
type window struct {
	key       string        // counter the request was added to
	ttl       time.Duration // ttl the counter was created with
	estimate  float64       // requests in the last Expiration, this one included
	reset     time.Duration // until the current window ends
	retryWait time.Duration // until one more request would be allowed
}

func (l *limiter) handle(c *qinetic.Ctx) error {
	if l.cfg.Next != nil && l.cfg.Next(c) {
		return c.Next()
	}
	ctx := c.Context()
	key := keyPrefix + l.cfg.KeyGenerator(c)
	var w window
	var err error
	if l.cfg.Algorithm == SlidingWindow {
		w, err = l.sliding(ctx, key)
	} else {
		w, err = l.fixed(ctx, key)
	}
	if err != nil {
		return err
	}
	limit := float64(l.cfg.Max)
	c.Set("X-RateLimit-Limit", strconv.Itoa(l.cfg.Max))
	c.Set("X-RateLimit-Reset", strconv.Itoa(ceilSeconds(w.reset)))
	if w.estimate > limit {
		// A rejected request does not use up the quota, or a client retrying in a loop stays blocked.
		if _, _, err := l.ctr.incr(ctx, w.key, -1, w.ttl); err != nil {
			return err
		}
		c.Set("X-RateLimit-Remaining", "0")
		c.Set("Retry-After", strconv.Itoa(max(1, ceilSeconds(w.retryWait))))
		return l.cfg.LimitReached(c)
	}
	c.Set("X-RateLimit-Remaining", strconv.Itoa(int(math.Max(0, math.Floor(limit-w.estimate)))))

	err = c.Next()
	if l.cfg.SkipFailedRequests || l.cfg.SkipSuccessfulRequests {
		failed := statusOf(c, err) >= 400
		if failed && l.cfg.SkipFailedRequests || !failed && l.cfg.SkipSuccessfulRequests {
			if _, _, derr := l.ctr.incr(ctx, w.key, -1, w.ttl); derr != nil {
				return errors.Join(err, derr)
			}
		}
	}
	return err
}

// statusOf is the status the client will get: the error's when the chain failed, since the ErrorHandler
// has not run yet.
func statusOf(c *qinetic.Ctx, err error) int {
	if err == nil {
		return c.StatusCode()
	}
	if e, ok := errors.AsType[*qinetic.Error](err); ok {
		return e.Code
	}
	return 500
}

func (l *limiter) fixed(ctx context.Context, key string) (window, error) {
	n, exp, err := l.ctr.incr(ctx, key, 1, l.cfg.Expiration)
	if err != nil {
		return window{}, err
	}
	reset := max(0, exp.Sub(now()))
	return window{key: key, ttl: l.cfg.Expiration, estimate: float64(n), reset: reset, retryWait: reset}, nil
}

// sliding approximates the count over the last Expiration with two aligned windows: the current one
// counted in full and the previous one weighted by how much of it the last Expiration still covers.
func (l *limiter) sliding(ctx context.Context, key string) (window, error) {
	e := l.cfg.Expiration
	t := now()
	idx := t.UnixNano() / int64(e)
	elapsed := time.Duration(t.UnixNano() - idx*int64(e))
	currKey := key + ":" + strconv.FormatInt(idx, 10)
	// Each window is read as the previous one for a whole Expiration after it ends.
	curr, _, err := l.ctr.incr(ctx, currKey, 1, 2*e)
	if err != nil {
		return window{}, err
	}
	prev, err := l.ctr.get(ctx, key+":"+strconv.FormatInt(idx-1, 10))
	if err != nil {
		return window{}, err
	}
	weight := 1 - float64(elapsed)/float64(e)
	w := window{
		key: currKey, ttl: 2 * e,
		estimate:  float64(curr) + float64(prev)*weight,
		reset:     e - elapsed,
		retryWait: retryAfter(float64(l.cfg.Max), float64(curr-1), float64(prev), elapsed, e),
	}
	return w, nil
}

// retryAfter returns how long until a request is allowed again: until curr + prev×weight + 1 ≤ limit.
// curr excludes the rejected request.
func retryAfter(limit, curr, prev float64, elapsed, e time.Duration) time.Duration {
	room := limit - 1 - curr
	if room >= 0 {
		// The current window has room; wait for the previous one's weight to fall far enough.
		if prev <= room {
			return 0
		}
		at := time.Duration((1 - room/prev) * float64(e)) // elapsed time at which prev×weight = room
		return max(0, at-elapsed)
	}
	// The current window alone is full: wait for the next one, where it becomes the previous window
	// and its weight has to fall until a request fits.
	return e - elapsed + time.Duration((1-(limit-1)/curr)*float64(e))
}

func ceilSeconds(d time.Duration) int {
	return int((d + time.Second - 1) / time.Second)
}

// counter adds to and reads integer counters in a Storage.
type counter interface {
	incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, time.Time, error)
	get(ctx context.Context, key string) (int64, error)
}

type atomicCounter struct {
	s   qinetic.Storage
	inc qinetic.Incrementer
}

func (a atomicCounter) incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, time.Time, error) {
	return a.inc.Incr(ctx, key, delta, ttl)
}

func (a atomicCounter) get(ctx context.Context, key string) (int64, error) {
	b, err := a.s.Get(ctx, key)
	if err != nil || b == nil {
		return 0, err
	}
	return strconv.ParseInt(string(b), 10, 64)
}

// lockedCounter emulates Incr with Get and Set for a Storage without it, serialized by a fixed set of
// mutexes picked by key hash, so the number of locks does not grow with the number of clients. It is exact
// within one process only. Values are "count:expiry in Unix nanoseconds".
type lockedCounter struct {
	s     qinetic.Storage
	locks [256]sync.Mutex
}

func (l *lockedCounter) lock(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &l.locks[h.Sum32()%uint32(len(l.locks))]
}

func (l *lockedCounter) incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, time.Time, error) {
	mu := l.lock(key)
	mu.Lock()
	defer mu.Unlock()
	t := now()
	n, exp, err := l.read(ctx, key, t)
	if err != nil {
		return 0, time.Time{}, err
	}
	if exp.IsZero() {
		exp = t.Add(ttl)
	}
	n += delta
	val := strconv.FormatInt(n, 10) + ":" + strconv.FormatInt(exp.UnixNano(), 10)
	if err := l.s.Set(ctx, key, []byte(val), exp.Sub(t)); err != nil {
		return 0, time.Time{}, err
	}
	return n, exp, nil
}

func (l *lockedCounter) get(ctx context.Context, key string) (int64, error) {
	n, _, err := l.read(ctx, key, now())
	return n, err
}

// read returns the counter at key, or zero with a zero expiry when it is missing or expired.
func (l *lockedCounter) read(ctx context.Context, key string, t time.Time) (int64, time.Time, error) {
	b, err := l.s.Get(ctx, key)
	if err != nil || b == nil {
		return 0, time.Time{}, err
	}
	countStr, expStr, ok := strings.Cut(string(b), ":")
	n, err1 := strconv.ParseInt(countStr, 10, 64)
	ns, err2 := strconv.ParseInt(expStr, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return 0, time.Time{}, errors.New("qinetic/limiter: malformed counter in storage")
	}
	exp := time.Unix(0, ns)
	if !t.Before(exp) {
		return 0, time.Time{}, nil
	}
	return n, exp, nil
}
