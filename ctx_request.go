package qinetic

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/erizeta/qilat"
)

// Context returns the request context. It is canceled when the client disconnects or the request
// finishes, and unlike the Ctx it may be kept after the handler returns.
func (c *Ctx) Context() context.Context {
	c.live()
	return c.r.Context()
}

// Protocol returns the request protocol: "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", or "HTTP/3.0".
func (c *Ctx) Protocol() string {
	c.live()
	return c.r.Proto()
}

// Request returns the underlying qilat request, valid until the handler chain returns.
func (c *Ctx) Request() *qilat.Request {
	c.live()
	return c.r
}

// Method returns the request method.
func (c *Ctx) Method() string {
	c.live()
	return c.r.Method()
}

// Path returns a copy of the percent-decoded request path. Routing uses the raw path, where an
// encoded slash (%2F) does not separate segments; here it is decoded to '/'.
func (c *Ctx) Path() string {
	c.live()
	return string(c.r.URI().Path())
}

// OriginalURL returns a copy of the request-target as received, with its query.
func (c *Ctx) OriginalURL() string {
	c.live()
	return string(c.r.URI().Raw())
}

// Host returns the host the client asked for, including any port: the Host header (:authority on HTTP/2
// and HTTP/3), or, when the connection comes from one of Config.TrustedProxies, the last X-Forwarded-Host
// value, which the nearest proxy set. Earlier values could have come from the client.
func (c *Ctx) Host() string {
	c.live()
	if c.app.trusted(c.remoteIP()) {
		if vals := c.r.Header.PeekAll("X-Forwarded-Host"); len(vals) > 0 {
			last := vals[len(vals)-1]
			if i := bytes.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if h := bytes.TrimSpace(last); len(h) > 0 {
				return string(h)
			}
		}
	}
	return string(c.r.Host())
}

// Hostname returns Host without its port, and without brackets for an IPv6 address.
func (c *Ctx) Hostname() string {
	c.live()
	host := c.Host()
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if len(host) > 1 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}
	return host
}

// Params returns a decoded copy of the route parameter key, or def[0] (or "") when it is absent or empty.
// Wildcards are named "*" (or "*1"), "*2", "+", and so on.
func (c *Ctx) Params(key string, def ...string) string {
	c.live()
	v := c.PeekParam(key)
	if len(v) == 0 {
		return first(def)
	}
	return string(v)
}

// PeekParam is like Params but returns the decoded value without copying it, or nil when it is absent.
// The result is valid only until the handler chain returns.
func (c *Ctx) PeekParam(key string) []byte {
	c.live()
	if c.route == nil {
		return nil
	}
	i := paramIndex(c.route.pat.names, key)
	if i < 0 {
		return nil
	}
	off := c.m.params[i]
	raw := c.m.path[off[0]:off[1]]
	if bytes.IndexByte(raw, '%') < 0 {
		return raw
	}
	start := len(c.scratch)
	for j := 0; j < len(raw); {
		b, w := decodeAt(raw, j)
		c.scratch = append(c.scratch, b)
		j += w
	}
	return c.scratch[start:len(c.scratch):len(c.scratch)]
}

func paramIndex(names []string, key string) int {
	switch key {
	case "*":
		key = "*1"
	case "+":
		key = "+1"
	}
	for i, n := range names {
		if n == key {
			return i
		}
	}
	return -1
}

// ParamsInt parses the route parameter key as an int.
func (c *Ctx) ParamsInt(key string) (int, error) {
	c.live()
	return strconv.Atoi(string(c.PeekParam(key)))
}

// Query returns a copy of the first query argument key, or def[0] (or "") when it is absent or empty.
func (c *Ctx) Query(key string, def ...string) string {
	c.live()
	v := c.r.URI().Query().Peek(key)
	if len(v) == 0 {
		return first(def)
	}
	return string(v)
}

// PeekQuery is like Query but returns the value without copying it, or nil when it is absent.
// The result is valid only until the handler chain returns.
func (c *Ctx) PeekQuery(key string) []byte {
	c.live()
	return c.r.URI().Query().Peek(key)
}

// QueryInt parses the query argument key as an int, returning def[0] (or 0) when it is absent or invalid.
func (c *Ctx) QueryInt(key string, def ...int) int {
	c.live()
	n, err := strconv.Atoi(string(c.r.URI().Query().Peek(key)))
	if err != nil {
		return first(def)
	}
	return n
}

// QueryBool parses the query argument key with strconv.ParseBool, returning def[0] (or false) when it
// is absent or invalid.
func (c *Ctx) QueryBool(key string, def ...bool) bool {
	c.live()
	b, err := strconv.ParseBool(string(c.r.URI().Query().Peek(key)))
	if err != nil {
		return first(def)
	}
	return b
}

// Queries returns copies of all query arguments, keeping the first value of repeated keys.
func (c *Ctx) Queries() map[string]string {
	c.live()
	q := c.r.URI().Query()
	m := make(map[string]string, q.Len())
	for k, v := range q.All() {
		if _, ok := m[string(k)]; !ok {
			m[string(k)] = string(v)
		}
	}
	return m
}

// Get returns a copy of the request header key, or def[0] (or "") when it is absent or empty.
func (c *Ctx) Get(key string, def ...string) string {
	c.live()
	v := c.r.Header.Peek(key)
	if len(v) == 0 {
		return first(def)
	}
	return string(v)
}

// PeekHeader is like Get but returns the value without copying it, or nil when it is absent.
// The result is valid only until the handler chain returns.
func (c *Ctx) PeekHeader(key string) []byte {
	c.live()
	return c.r.Header.Peek(key)
}

// Cookies returns the value of the request cookie key, or def[0] (or "") when it is absent or empty.
func (c *Ctx) Cookies(key string, def ...string) string {
	c.live()
	// HTTP/2 and HTTP/3 clients may split cookies over several Cookie fields.
	for _, line := range c.r.Header.PeekAll("Cookie") {
		cookies, err := http.ParseCookie(string(line))
		if err != nil {
			continue
		}
		for _, ck := range cookies {
			if ck.Name == key && ck.Value != "" {
				return ck.Value
			}
		}
	}
	return first(def)
}

// IP returns the client IP. It is the connection's remote address unless that address is one of
// Config.TrustedProxies: then it is the rightmost address in Config.ProxyHeader that is not a trusted
// proxy, so a client cannot spoof it by sending the header itself.
func (c *Ctx) IP() string {
	c.live()
	remote := c.remoteIP()
	if c.app.config.ProxyHeader == "" || !c.app.trusted(remote) {
		return addrString(remote)
	}
	values := c.r.Header.PeekAll(c.app.config.ProxyHeader)
	ip := remote
	for i := len(values) - 1; i >= 0; i-- {
		list := string(values[i])
		for list != "" {
			var entry string
			if j := strings.LastIndexByte(list, ','); j >= 0 {
				list, entry = list[:j], list[j+1:]
			} else {
				list, entry = "", list
			}
			addr, err := netip.ParseAddr(strings.TrimSpace(entry))
			if err != nil {
				// A malformed hop means the rest of the list cannot be trusted either.
				return addrString(ip)
			}
			ip = addr.Unmap()
			if !c.app.trusted(ip) {
				return ip.String()
			}
		}
	}
	return addrString(ip)
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func (c *Ctx) remoteIP() netip.Addr {
	switch a := c.r.RemoteAddr().(type) {
	case *net.TCPAddr:
		return a.AddrPort().Addr().Unmap()
	case *net.UDPAddr:
		return a.AddrPort().Addr().Unmap()
	case nil:
		return netip.Addr{}
	default:
		ap, err := netip.ParseAddrPort(a.String())
		if err != nil {
			addr, _ := netip.ParseAddr(a.String())
			return addr.Unmap()
		}
		return ap.Addr().Unmap()
	}
}

// IPs returns the comma-separated entries of the proxy header (Config.ProxyHeader, or X-Forwarded-For),
// as sent. They come from the client or proxies and must not be trusted.
func (c *Ctx) IPs() []string {
	c.live()
	name := c.app.config.ProxyHeader
	if name == "" {
		name = "X-Forwarded-For"
	}
	var ips []string
	for _, v := range c.r.Header.PeekAll(name) {
		for entry := range strings.SplitSeq(string(v), ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				ips = append(ips, entry)
			}
		}
	}
	return ips
}

// Secure reports whether the request came over TLS, directly or, from a trusted proxy, as declared by
// X-Forwarded-Proto: https.
func (c *Ctx) Secure() bool {
	c.live()
	if c.r.TLS() != nil {
		return true
	}
	return c.app.trusted(c.remoteIP()) && strings.EqualFold(string(c.r.Header.Peek("X-Forwarded-Proto")), "https")
}

// Body reads the whole request body, once; later calls return the same bytes. It fails with
// ErrRequestEntityTooLarge past Config.BodyLimit and with a 400 *Error when reading fails.
// The result is valid only until the handler chain returns.
func (c *Ctx) Body() ([]byte, error) {
	c.live()
	if !c.bodyRead {
		c.bodyRead = true
		c.reqBody, c.bodyErr = c.readBody(c.reqBody[:0])
	}
	return c.reqBody, c.bodyErr
}

func (c *Ctx) readBody(buf []byte) ([]byte, error) {
	limit := c.app.bodyLimit
	if limit >= 0 && c.r.ContentLength() > limit {
		return buf, ErrRequestEntityTooLarge
	}
	r := c.r.Body
	if limit >= 0 {
		r = io.LimitReader(r, limit+1)
	}
	if n := c.r.ContentLength(); n > 0 {
		buf = slices.Grow(buf, int(n)+1) // +1 to see EOF without growing again
	}
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, 512)
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if limit >= 0 && int64(len(buf)) > limit {
			return buf[:0], ErrRequestEntityTooLarge
		}
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if errors.Is(err, qilat.ErrBodyTooLarge) {
			return buf[:0], ErrRequestEntityTooLarge
		}
		if err != nil {
			return buf[:0], clientError(http.StatusBadRequest, "Bad Request", err)
		}
	}
}

// BodyReader returns the request body as a stream, limited like Body. After Body it reads the bytes
// Body returned. Do not mix it with Body otherwise.
func (c *Ctx) BodyReader() io.Reader {
	c.live()
	if c.bodyRead {
		if c.bodyErr != nil {
			return errReader{c.bodyErr}
		}
		return bytes.NewReader(c.reqBody)
	}
	if c.app.bodyLimit < 0 {
		return c.r.Body
	}
	if c.r.ContentLength() > c.app.bodyLimit {
		return errReader{ErrRequestEntityTooLarge}
	}
	return &limitedReader{r: c.r.Body, left: c.app.bodyLimit}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// limitedReader fails with ErrRequestEntityTooLarge once more than left bytes arrive.
type limitedReader struct {
	r    io.Reader
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if int64(len(p)) > l.left+1 {
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	if int64(n) > l.left {
		return int(l.left), ErrRequestEntityTooLarge
	}
	l.left -= int64(n)
	if errors.Is(err, qilat.ErrBodyTooLarge) {
		err = ErrRequestEntityTooLarge
	}
	return n, err
}

// FormValue returns a copy of the first value of the form field key from an
// application/x-www-form-urlencoded or multipart/form-data body, or def[0] (or "") when it is absent,
// empty, or the body cannot be read. The query string is not consulted; use Query for it.
func (c *Ctx) FormValue(key string, def ...string) string {
	c.live()
	if err := c.parseForm(); err != nil {
		return first(def)
	}
	if c.mform != nil {
		if vs := c.mform.Value[key]; len(vs) > 0 && vs[0] != "" {
			return vs[0]
		}
		return first(def)
	}
	if v := c.form.Peek(key); len(v) > 0 {
		return string(v)
	}
	return first(def)
}

// MultipartForm parses a multipart/form-data body. It is read in memory, bounded by Config.BodyLimit.
func (c *Ctx) MultipartForm() (*multipart.Form, error) {
	c.live()
	if err := c.parseForm(); err != nil {
		return nil, err
	}
	if c.mform == nil {
		return nil, clientError(http.StatusBadRequest, "Bad Request", errors.New("qinetic: not a multipart/form-data body"))
	}
	return c.mform, nil
}

// FormFile returns the first file uploaded in the multipart form field key.
func (c *Ctx) FormFile(key string) (*multipart.FileHeader, error) {
	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}
	if fs := form.File[key]; len(fs) > 0 {
		return fs[0], nil
	}
	return nil, clientError(http.StatusBadRequest, "Bad Request", errors.New("qinetic: no file in form field "+strconv.Quote(key)))
}

func (c *Ctx) parseForm() error {
	if c.formRead {
		return c.formErr
	}
	c.formRead = true
	c.formErr = c.readForm()
	return c.formErr
}

func (c *Ctx) readForm() error {
	mediaType, params, err := mime.ParseMediaType(string(c.r.Header.Peek("Content-Type")))
	if err != nil {
		return ErrUnsupportedMediaType
	}
	switch mediaType {
	case "application/x-www-form-urlencoded":
		body, err := c.Body()
		if err != nil {
			return err
		}
		c.form.Parse(body)
		return nil
	case "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return clientError(http.StatusBadRequest, "Bad Request", errors.New("qinetic: multipart boundary missing"))
		}
		body, err := c.Body()
		if err != nil {
			return err
		}
		// The body is already in memory, so the whole form fits within it and no part spills to disk.
		form, err := multipart.NewReader(bytes.NewReader(body), boundary).ReadForm(int64(len(body)) + 1)
		if err != nil {
			return clientError(http.StatusBadRequest, "Bad Request", err)
		}
		c.mform = form
		return nil
	}
	return ErrUnsupportedMediaType
}

// Locals stores value under key for later handlers of the same request, or returns the stored value
// when value is omitted.
func (c *Ctx) Locals(key any, value ...any) any {
	c.live()
	if len(value) == 0 {
		return c.locals[key]
	}
	if c.locals == nil {
		c.locals = make(map[any]any)
	}
	c.locals[key] = value[0]
	return value[0]
}

// Route returns a copy of the route being run, or the zero Route before any route matched. Changing it
// does not change the route; its Params slice is shared and must not be modified.
func (c *Ctx) Route() Route {
	c.live()
	if c.route == nil {
		return Route{}
	}
	return c.route.info
}

func first[T any](def []T) T {
	if len(def) > 0 {
		return def[0]
	}
	var zero T
	return zero
}
