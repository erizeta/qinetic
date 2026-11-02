// Package cache stores responses and serves them again, keeping a separate copy per value of each header
// the response varies on, so a response for one Origin or language is never served to another.
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/erizeta/qilat"
	"github.com/erizeta/qinetic"
	"github.com/erizeta/qinetic/internal/memory"
)

// Config configures the middleware.
type Config struct {
	// Next skips the middleware when it returns true.
	Next func(c *qinetic.Ctx) bool
	// Expiration is how long a response is kept. If zero, one minute.
	Expiration time.Duration
	// KeyGenerator returns the key a response is stored under. If nil, the host, path, and raw query.
	KeyGenerator func(c *qinetic.Ctx) string
	// StatusCodes lists the statuses stored. If nil, only 200.
	StatusCodes []int
	// MaxBytes bounds a stored body. If zero, 1 MiB; larger responses are not stored.
	MaxBytes int
	// MaxVaryHeaders bounds the headers a response may vary on and still be stored. If zero, 8.
	MaxVaryHeaders int
	// MaxVariants bounds the copies kept per key, so random header values cannot fill the storage. If zero, 16.
	MaxVariants int
	// CacheHeader names the response header set to "hit" or "miss". If empty, X-Cache.
	CacheHeader string
	// Storage holds the responses. If nil, an in-memory storage per middleware holding at most 64 MiB.
	Storage qinetic.Storage
	// Logger receives storage errors. The request is then served without the cache, so an outage of an
	// external storage does not fail requests. If nil, slog.Default is used.
	Logger *slog.Logger
}

// now is the clock, replaced in tests.
var now = time.Now

const (
	defaultMemoryBytes = 64 << 20
	keyPrefix          = "cache:"
	formatVersion      = 1
	kindEntry          = 'E'
	kindManifest       = 'M'
)

// unstored are response headers a stored copy must not replay: framing, recomputed for each response, or
// set per request.
var unstored = []string{"connection", "keep-alive", "transfer-encoding", "content-length", "date", "set-cookie", "x-cache"}

// New returns the middleware. Only GET responses are stored; GET and HEAD requests are served from them.
// It stores nothing for a request with Authorization or Cache-Control: no-store, nor for a response with
// Set-Cookie, Cache-Control private, no-store, or no-cache, Vary: *, a committed (streamed) body, or more
// than MaxVaryHeaders or MaxVariants. Headers set by middleware before this one are not stored, since
// they run again on every hit. Register it after compress: before compress it sees no body to store.
func New(config ...Config) qinetic.Handler {
	var cfg Config
	if len(config) > 0 {
		cfg = config[0]
	}
	if cfg.Expiration <= 0 {
		cfg.Expiration = time.Minute
	}
	if cfg.KeyGenerator == nil {
		cfg.KeyGenerator = func(c *qinetic.Ctx) string {
			return c.Hostname() + string(c.Request().URI().RawPath()) + "?" + string(c.Request().URI().RawQuery())
		}
	}
	if cfg.StatusCodes == nil {
		cfg.StatusCodes = []int{http.StatusOK}
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 20
	}
	if cfg.MaxVaryHeaders <= 0 {
		cfg.MaxVaryHeaders = 8
	}
	if cfg.MaxVariants <= 0 {
		cfg.MaxVariants = 16
	}
	if cfg.CacheHeader == "" {
		cfg.CacheHeader = "X-Cache"
	}
	if cfg.Storage == nil {
		cfg.Storage = memory.New(memory.Config{MaxBytes: defaultMemoryBytes, Clock: func() time.Time { return now() }})
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &middleware{cfg: cfg}
	return m.handle
}

type middleware struct{ cfg Config }

type field struct{ name, value string }

// entry is a stored response.
type entry struct {
	status int
	header []field
	body   []byte
}

// manifest lists the headers the responses under a key vary on, and how many copies are stored.
type manifest struct {
	names    []string
	variants int
}

func (m *middleware) handle(c *qinetic.Ctx) error {
	if m.cfg.Next != nil && m.cfg.Next(c) {
		return c.Next()
	}
	method := c.Method()
	if method != http.MethodGet && method != http.MethodHead {
		return c.Next()
	}
	if c.Get("Authorization") != "" || hasToken(c.Get("Cache-Control"), "no-store") {
		return c.Next()
	}
	base := hashKey(m.cfg.KeyGenerator(c))
	e, err := m.lookup(c, base)
	if err != nil {
		m.logError(c, "read", err)
	} else if e != nil {
		return m.serve(c, e)
	}

	before := snapshot(c)
	if err := c.Next(); err != nil {
		return err
	}
	c.Set(m.cfg.CacheHeader, "miss")
	if method == http.MethodHead {
		// A HEAD handler may send no body; storing it would serve an empty body to GET requests.
		return nil
	}
	if err := m.store(c, base, before); err != nil {
		m.logError(c, "write", err)
	}
	return nil
}

func (m *middleware) logError(c *qinetic.Ctx, op string, err error) {
	m.cfg.Logger.LogAttrs(c.Context(), slog.LevelError, "qinetic/cache: storage "+op+" failed",
		slog.String("path", string(c.Request().URI().RawPath())), slog.Any("error", err))
}

func (m *middleware) lookup(c *qinetic.Ctx, base string) (*entry, error) {
	ctx := c.Context()
	b, err := m.cfg.Storage.Get(ctx, base)
	if err != nil || b == nil {
		return nil, err
	}
	kind, payload, ok := header(b)
	switch {
	case !ok:
		return nil, nil
	case kind == kindEntry:
		return decodeEntry(payload), nil
	case kind != kindManifest:
		return nil, nil
	}
	man, ok := decodeManifest(payload)
	if !ok {
		return nil, nil
	}
	b, err = m.cfg.Storage.Get(ctx, variantKey(base, c, man.names))
	if err != nil || b == nil {
		return nil, err
	}
	if kind, payload, ok = header(b); !ok || kind != kindEntry {
		return nil, nil
	}
	return decodeEntry(payload), nil
}

func (m *middleware) serve(c *qinetic.Ctx, e *entry) error {
	h := c.ResponseHeader()
	for _, f := range e.header {
		if !containsBytes(h.PeekAll(f.name), f.value) {
			h.Add(f.name, f.value)
		}
	}
	c.Set(m.cfg.CacheHeader, "hit")
	return c.Status(e.status).Send(e.body)
}

func (m *middleware) store(c *qinetic.Ctx, base string, before map[string][]string) error {
	status := c.StatusCode()
	if !slices.Contains(m.cfg.StatusCodes, status) || c.Committed() {
		return nil
	}
	body := c.ResponseBody()
	h := c.ResponseHeader()
	if len(body) > m.cfg.MaxBytes || h.Has("Set-Cookie") {
		return nil
	}
	cc := string(h.Peek("Cache-Control"))
	if hasToken(cc, "private") || hasToken(cc, "no-store") || hasToken(cc, "no-cache") {
		return nil
	}
	names, ok := varyNames(h)
	if !ok || len(names) > m.cfg.MaxVaryHeaders {
		return nil
	}
	e := &entry{status: status, body: body}
	for k, v := range h.All() {
		name := strings.ToLower(string(k))
		if slices.Contains(unstored, name) || slices.Contains(before[name], string(v)) {
			continue
		}
		e.header = append(e.header, field{string(k), string(v)})
	}
	ctx := c.Context()
	ttl := m.cfg.Expiration
	if len(names) == 0 {
		return m.cfg.Storage.Set(ctx, base, encodeEntry(e), ttl)
	}
	vkey := variantKey(base, c, names)
	man := manifest{names: names, variants: 1}
	if b, err := m.cfg.Storage.Get(ctx, base); err != nil {
		return err
	} else if kind, payload, ok := header(b); ok && kind == kindManifest {
		if old, ok := decodeManifest(payload); ok && slices.Equal(old.names, names) {
			existing, err := m.cfg.Storage.Get(ctx, vkey)
			if err != nil {
				return err
			}
			man.variants = old.variants
			if existing == nil {
				if old.variants >= m.cfg.MaxVariants {
					return nil
				}
				man.variants++
			}
		}
	}
	// The variant goes first, so a manifest never points at a copy that was not written.
	if err := m.cfg.Storage.Set(ctx, vkey, encodeEntry(e), ttl); err != nil {
		return err
	}
	return m.cfg.Storage.Set(ctx, base, encodeManifest(man), ttl)
}

// snapshot records the response headers set before the handlers after the cache run, so a stored copy
// leaves them out: those handlers set them again on every hit, possibly to other values.
func snapshot(c *qinetic.Ctx) map[string][]string {
	h := c.ResponseHeader()
	if h.Len() == 0 {
		return nil
	}
	m := make(map[string][]string, h.Len())
	for k, v := range h.All() {
		name := strings.ToLower(string(k))
		m[name] = append(m[name], string(v))
	}
	return m
}

func containsBytes(values [][]byte, v string) bool {
	for _, b := range values {
		if string(b) == v {
			return true
		}
	}
	return false
}

// varyNames returns the lower-case, sorted, distinct header names of the response's Vary, or false for
// Vary: *, which no stored copy can satisfy.
func varyNames(h *qilat.Header) ([]string, bool) {
	var names []string
	for _, line := range h.PeekAll("Vary") {
		for item := range strings.SplitSeq(string(line), ",") {
			name := strings.ToLower(strings.TrimSpace(item))
			switch {
			case name == "*":
				return nil, false
			case name != "" && !slices.Contains(names, name):
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names, true
}

// variantKey hashes the request's values of names onto base. An absent header and an empty one differ.
func variantKey(base string, c *qinetic.Ctx, names []string) string {
	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(name))
		lines := c.Request().Header.PeekAll(name)
		if lines == nil {
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		for i, line := range lines {
			if i > 0 {
				h.Write([]byte{','})
			}
			h.Write(bytes.Trim(line, " \t"))
		}
		h.Write([]byte{0})
	}
	return base + ":" + hex.EncodeToString(h.Sum(nil))
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return keyPrefix + hex.EncodeToString(sum[:])
}

func hasToken(list, token string) bool {
	for item := range strings.SplitSeq(list, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(item), "=")
		if strings.EqualFold(strings.TrimSpace(name), token) {
			return true
		}
	}
	return false
}

// Stored values: a version byte, a kind byte, then the payload. Strings and byte slices are
// length-prefixed with uvarints. Anything else, or another version, reads as a miss.

func header(b []byte) (kind byte, payload []byte, ok bool) {
	if len(b) < 2 || b[0] != formatVersion {
		return 0, nil, false
	}
	return b[1], b[2:], true
}

func encodeEntry(e *entry) []byte {
	b := []byte{formatVersion, kindEntry}
	b = appendInt(b, e.status)
	b = appendInt(b, len(e.header))
	for _, f := range e.header {
		b = appendString(b, f.name)
		b = appendString(b, f.value)
	}
	return appendString(b, string(e.body))
}

func decodeEntry(p []byte) *entry {
	r := reader{b: p}
	status := r.uvarint()
	// Stored data may come from an external store: range-check before converting.
	if status < 100 || status > 999 {
		return nil
	}
	e := &entry{status: int(status)}
	n := r.uvarint()
	for i := uint64(0); i < n && r.err == nil; i++ {
		e.header = append(e.header, field{r.string(), r.string()})
	}
	e.body = []byte(r.string())
	if r.err != nil || len(r.b) != 0 {
		return nil
	}
	return e
}

func encodeManifest(m manifest) []byte {
	b := []byte{formatVersion, kindManifest}
	b = appendInt(b, m.variants)
	b = appendInt(b, len(m.names))
	for _, n := range m.names {
		b = appendString(b, n)
	}
	return b
}

func decodeManifest(p []byte) (manifest, bool) {
	r := reader{b: p}
	variants := r.uvarint()
	if variants > math.MaxInt32 {
		return manifest{}, false
	}
	m := manifest{variants: int(variants)}
	n := r.uvarint()
	for i := uint64(0); i < n && r.err == nil; i++ {
		m.names = append(m.names, r.string())
	}
	return m, r.err == nil && len(r.b) == 0 && len(m.names) > 0
}

// appendInt appends a non-negative int as a uvarint; the values written, statuses, counts, and lengths,
// are never negative.
func appendInt(b []byte, v int) []byte {
	if v < 0 {
		v = 0
	}
	return binary.AppendUvarint(b, uint64(v))
}

func appendString(b []byte, s string) []byte {
	return append(appendInt(b, len(s)), s...)
}

var errCorrupt = errors.New("corrupt cache entry")

type reader struct {
	b   []byte
	err error
}

func (r *reader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.err = errCorrupt
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) string() string {
	n := r.uvarint()
	if r.err != nil || n > uint64(len(r.b)) {
		r.err = errCorrupt
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}
