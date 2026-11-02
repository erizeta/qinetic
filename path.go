package qinetic

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// maxParams bounds the parameters, wildcards included, one route pattern may declare.
const maxParams = 30

type segKind uint8

const (
	segStatic   segKind = iota
	segParam            // :name, exactly one non-empty segment
	segOptional         // :name?, last segment only; may be absent or empty
	segStar             // *, rest of the path when last, otherwise one or more segments
	segPlus             // +, like * but never empty
)

type segment struct {
	kind segKind
	// text is the decoded static text, folded to lower case for case-insensitive routing.
	text string
	// param is the index into pattern.names and the match offsets, or -1 for static segments.
	param int
}

func (s *segment) isWildcard() bool { return s.kind == segStar || s.kind == segPlus }

// pattern is a compiled route path. Each segment covers whole path segments, so params never span
// a '/' that is part of the path, and an escaped %2F always stays inside one segment.
type pattern struct {
	segs  []segment
	names []string
	// midWildcard is set when a wildcard is followed by more segments, which needs placement search.
	midWildcard bool
	fold        bool
}

// paramOffsets holds, for each parameter of the matched pattern, start and end offsets into the raw path.
type paramOffsets [maxParams][2]int

func isNameChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// normalizePattern applies Fiber's defaults: an empty or relative path is rooted, and without strict
// routing trailing slashes are dropped.
func normalizePattern(p string, strict bool) string {
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	if !strict {
		for len(p) > 1 && p[len(p)-1] == '/' {
			p = p[:len(p)-1]
		}
	}
	return p
}

func parsePattern(raw string, caseSensitive, strict bool) (*pattern, error) {
	p := normalizePattern(raw, strict)
	pat := &pattern{fold: !caseSensitive}
	if p == "/" {
		return pat, nil
	}
	parts := strings.Split(p[1:], "/")
	stars, pluses := 0, 0
	for i, part := range parts {
		last := i == len(parts)-1
		seg := segment{param: -1}
		switch {
		case part == "":
			// Only strict routing keeps a trailing slash; it becomes an empty last segment.
			if !last {
				return nil, errors.New("empty segment")
			}
			seg.kind = segStatic
		case part == "*" || part == "+":
			seg.kind = segStar
			stars++
			name := "*" + strconv.Itoa(stars)
			if part == "+" {
				seg.kind = segPlus
				stars--
				pluses++
				name = "+" + strconv.Itoa(pluses)
			}
			seg.param = len(pat.names)
			pat.names = append(pat.names, name)
			if !last {
				pat.midWildcard = true
			}
		case part[0] == ':':
			name := part[1:]
			seg.kind = segParam
			if strings.HasSuffix(name, "?") {
				name = name[:len(name)-1]
				seg.kind = segOptional
				if !last {
					return nil, fmt.Errorf("optional parameter %q must be the last segment", part)
				}
				if pat.hasWildcard() {
					return nil, fmt.Errorf("optional parameter %q cannot follow a wildcard", part)
				}
			}
			if name == "" {
				return nil, errors.New("parameter without a name")
			}
			for j := 0; j < len(name); j++ {
				if !isNameChar(name[j]) {
					return nil, fmt.Errorf("parameter %q: names use only letters, digits, and '_', one per segment", part)
				}
			}
			for _, n := range pat.names {
				if n == name {
					return nil, fmt.Errorf("duplicate parameter %q", name)
				}
			}
			seg.param = len(pat.names)
			pat.names = append(pat.names, name)
		default:
			if strings.ContainsAny(part, ":*+?") {
				return nil, fmt.Errorf("segment %q: ':', '*', '+', and '?' are only allowed as a whole segment; percent-encode them for a literal match", part)
			}
			text, ok := unescapeSegment(part)
			if !ok {
				return nil, fmt.Errorf("segment %q has an invalid percent-encoding", part)
			}
			if text == "." || text == ".." {
				return nil, fmt.Errorf("segment %q is a dot segment", part)
			}
			if pat.fold {
				text = toLowerASCII(text)
			}
			seg.kind, seg.text = segStatic, text
		}
		pat.segs = append(pat.segs, seg)
	}
	if len(pat.names) > maxParams {
		return nil, fmt.Errorf("more than %d parameters", maxParams)
	}
	return pat, nil
}

func (p *pattern) hasWildcard() bool {
	for i := range p.segs {
		if p.segs[i].isWildcard() {
			return true
		}
	}
	return false
}

// staticText returns the text of segment i when the first i+1 segments are static and non-empty,
// which makes it a router bucket key.
func (p *pattern) staticText(i int) (string, bool) {
	if len(p.segs) <= i {
		return "", false
	}
	for j := 0; j <= i; j++ {
		if p.segs[j].kind != segStatic || p.segs[j].text == "" {
			return "", false
		}
	}
	return p.segs[i].text, true
}

// match reports whether path matches p and records parameter offsets. path must be a clean routing path
// (see routingPath). With prefix, path may continue past the pattern at a segment boundary.
func (p *pattern) match(path []byte, prefix bool, params *paramOffsets) bool {
	if !prefix || !p.midWildcard {
		return p.matchFull(path, prefix, params)
	}
	// A middleware pattern with an inner wildcard can end at any segment boundary; prefer the longest.
	for end := len(path); end > 0; end-- {
		if (end == len(path) || path[end] == '/') && p.matchFull(path[:end], false, params) {
			return true
		}
	}
	return false
}

// Segment cursors: a cursor is the offset where the next path segment starts; len(path)+1 means none left.
func firstCursor(path []byte) int {
	if len(path) == 1 {
		return 2
	}
	return 1
}

func segEnd(path []byte, cur int) int {
	for i := cur; i < len(path); i++ {
		if path[i] == '/' {
			return i
		}
	}
	return len(path)
}

func (p *pattern) matchFull(path []byte, prefix bool, params *paramOffsets) bool {
	cur := firstCursor(path)
	segs := p.segs
	for i := range segs {
		s := &segs[i]
		if s.isWildcard() {
			if i == len(segs)-1 {
				return p.matchTail(path, cur, s, params)
			}
			return p.matchWildcards(path, cur, segs[i:], params)
		}
		if cur > len(path) {
			if s.kind == segOptional {
				params[s.param] = [2]int{len(path), len(path)}
				return true
			}
			return false
		}
		end := segEnd(path, cur)
		if !p.matchSeg(s, path, cur, end, params) {
			return false
		}
		cur = end + 1
	}
	return prefix || cur > len(path)
}

func (p *pattern) matchSeg(s *segment, path []byte, start, end int, params *paramOffsets) bool {
	switch s.kind {
	case segStatic:
		return segEqual(path[start:end], s.text, p.fold)
	case segParam:
		if start == end {
			return false
		}
	}
	params[s.param] = [2]int{start, end}
	return true
}

// matchTail matches a trailing wildcard against everything from cur.
func (p *pattern) matchTail(path []byte, cur int, s *segment, params *paramOffsets) bool {
	start := min(cur, len(path))
	if s.kind == segPlus && start == len(path) {
		return false
	}
	params[s.param] = [2]int{start, len(path)}
	return true
}

// matchWildcards matches segs, which starts with a wildcard, from cur to the end of path. Each fixed run
// after a wildcard is placed as far right as possible, so earlier wildcards are greedy, as in Fiber.
// Placing runs right to left needs no backtracking: the rightmost fit leaves the most room on the left.
func (p *pattern) matchWildcards(path []byte, cur int, segs []segment, params *paramOffsets) bool {
	type run struct{ wild, from, to int } // wildcard index in segs, fixed run segs[from:to]
	var runs [maxParams]run
	n := 0
	var last *run
	for i := range segs {
		if segs[i].isWildcard() {
			runs[n] = run{wild: i, from: i + 1, to: i + 1}
			last = &runs[n]
			n++
		} else if last != nil {
			last.to = i + 1
		}
	}
	if last == nil {
		return false
	}
	trailing := last.from == last.to
	// starts and ends are the cursors where each run begins and where the segment after it begins.
	var starts, ends [maxParams]int
	limit := len(path) + 1
	for k := n - 1; k >= 0; k-- {
		r := runs[k]
		if r.from == r.to {
			starts[k], ends[k] = limit, limit
			continue
		}
		maxEnd := limit
		if k < n-1 {
			next := &segs[runs[k+1].wild]
			// The wildcard after this run needs a segment, or a byte when it is a trailing +.
			if k+1 != n-1 || !trailing || next.kind != segStar {
				maxEnd = prevCursor(path, limit)
			}
		}
		placed := false
		for end := maxEnd; end > cur; end = prevCursor(path, end) {
			if start, ok := p.matchRunBefore(path, cur, end, segs[r.from:r.to], params); ok {
				starts[k], ends[k], placed = start, end, true
				break
			}
			if k == n-1 {
				break // the last run is anchored at the end of the path
			}
		}
		if !placed {
			return false
		}
		limit = starts[k]
	}
	from := cur
	for k := 0; k < n; k++ {
		s := &segs[runs[k].wild]
		if k == n-1 && trailing {
			start := min(from, len(path))
			if s.kind == segPlus && start == len(path) {
				return false
			}
			params[s.param] = [2]int{start, len(path)}
			break
		}
		if starts[k] <= from {
			return false
		}
		params[s.param] = [2]int{from, starts[k] - 1}
		from = ends[k]
	}
	return true
}

// prevCursor returns the cursor of the segment before the one at cur.
func prevCursor(path []byte, cur int) int {
	i := cur - 2
	for i >= 0 && path[i] != '/' {
		i--
	}
	return i + 1
}

// matchRunBefore matches run against the segments ending just before cursor end, all at or after lower.
// It returns the cursor where the run starts.
func (p *pattern) matchRunBefore(path []byte, lower, end int, run []segment, params *paramOffsets) (int, bool) {
	cur := end
	for i := len(run) - 1; i >= 0; i-- {
		if cur <= lower {
			return 0, false
		}
		segStart := prevCursor(path, cur)
		if segStart < lower || !p.matchSeg(&run[i], path, segStart, cur-1, params) {
			return 0, false
		}
		cur = segStart
	}
	return cur, true
}

// routingPath checks that raw is a clean, origin-form path and returns the part that is matched: without
// strict routing, one trailing slash is ignored. Empty or dot segments, encoded or not, are never routed.
func routingPath(raw []byte, strict bool) ([]byte, bool) {
	if len(raw) == 0 || raw[0] != '/' {
		return nil, false
	}
	if len(raw) == 1 {
		return raw, true
	}
	path := raw
	if !strict && path[len(path)-1] == '/' {
		path = path[:len(path)-1]
	}
	for cur := 1; cur <= len(path); {
		end := segEnd(path, cur)
		seg := path[cur:end]
		if len(seg) == 0 {
			if end != len(path) || !strict {
				return nil, false
			}
		} else if isDotSegment(seg) {
			return nil, false
		}
		cur = end + 1
	}
	return path, true
}

func isDotSegment(seg []byte) bool {
	if len(seg) > 6 {
		return false
	}
	return segEqual(seg, ".", false) || segEqual(seg, "..", false)
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// decodeAt returns the byte at raw[i] with a %XX escape decoded, and the width consumed.
// A malformed escape is taken literally.
func decodeAt(raw []byte, i int) (byte, int) {
	if raw[i] == '%' && i+2 < len(raw) {
		hi, ok1 := unhex(raw[i+1])
		lo, ok2 := unhex(raw[i+2])
		if ok1 && ok2 {
			return hi<<4 | lo, 3
		}
	}
	return raw[i], 1
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func toLowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				b[j] = lowerASCII(b[j])
			}
			return string(b)
		}
	}
	return s
}

// segEqual compares a raw path segment, decoding escapes as it goes, with decoded text that is already
// folded when fold is set.
func segEqual(raw []byte, text string, fold bool) bool {
	j := 0
	for i := 0; i < len(raw); {
		c, w := decodeAt(raw, i)
		if fold {
			c = lowerASCII(c)
		}
		if j >= len(text) || text[j] != c {
			return false
		}
		i += w
		j++
	}
	return j == len(text)
}

// unescapeSegment decodes a pattern segment; unlike path matching it rejects malformed escapes.
func unescapeSegment(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", false
		}
		hi, ok1 := unhex(s[i+1])
		lo, ok2 := unhex(s[i+2])
		if !ok1 || !ok2 {
			return "", false
		}
		b = append(b, hi<<4|lo)
		i += 2
	}
	return string(b), true
}
