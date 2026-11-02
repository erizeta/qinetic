package qinetic

import (
	"net/http"
	"strings"
	"time"
)

// SameSite values for Cookie.SameSite.
const (
	CookieSameSiteLax    = "Lax"
	CookieSameSiteStrict = "Strict"
	CookieSameSiteNone   = "None"
)

// Cookie is a response cookie, serialized with net/http.
type Cookie struct {
	Name        string
	Value       string
	Path        string
	Domain      string
	MaxAge      int // seconds; zero omits Max-Age, negative deletes the cookie now
	Expires     time.Time
	Secure      bool
	HTTPOnly    bool
	SameSite    string // CookieSameSiteLax, CookieSameSiteStrict, CookieSameSiteNone, or "" to omit
	Partitioned bool
}

func (ck *Cookie) http() *http.Cookie {
	hc := &http.Cookie{ //nolint:gosec // Secure, HttpOnly, and SameSite are the caller's choice
		Name: ck.Name, Value: ck.Value, Path: ck.Path, Domain: ck.Domain, MaxAge: ck.MaxAge,
		Expires: ck.Expires, Secure: ck.Secure, HttpOnly: ck.HTTPOnly, Partitioned: ck.Partitioned,
	}
	switch strings.ToLower(ck.SameSite) {
	case "lax":
		hc.SameSite = http.SameSiteLaxMode
	case "strict":
		hc.SameSite = http.SameSiteStrictMode
	case "none":
		hc.SameSite = http.SameSiteNoneMode
	}
	return hc
}

// Cookie adds a Set-Cookie header. A cookie net/http considers invalid, such as one whose name or value
// has characters that could inject attributes, is not sent; the reason goes to Config.Logger.
func (c *Ctx) Cookie(ck *Cookie) {
	c.live()
	hc := ck.http()
	if err := hc.Valid(); err != nil {
		c.app.logError(c, "invalid cookie not sent", err)
		return
	}
	c.w.Header().Add("Set-Cookie", hc.String())
}

// ClearCookie expires the named cookies in the client, or every cookie of the request when no name is given.
// Cookies set with a Path or Domain are cleared only by a Cookie call with the same Path and Domain.
func (c *Ctx) ClearCookie(names ...string) {
	c.live()
	if len(names) == 0 {
		for _, line := range c.r.Header.PeekAll("Cookie") {
			cookies, err := http.ParseCookie(string(line))
			if err != nil {
				continue
			}
			for _, ck := range cookies {
				names = append(names, ck.Name)
			}
		}
	}
	for _, name := range names {
		c.Cookie(&Cookie{Name: name, MaxAge: -1, Expires: time.Unix(0, 0)})
	}
}
