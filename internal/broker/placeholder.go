package broker

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// A session token is the guest's handle on a route. The tool in the guest
// presents it in one place, the carrier the route reads it from, such as
// Authorization: Bearer. Found anywhere else in a request, in the path, the
// query, another header or the body, plain or encoded, it is a handle the
// agent is leaking, and the request is refused before any credential is read.
// docs/broker.md has the positions and encodings this finds, and its limits.

// maxLive bounds the session tokens a sandbox keeps for the scan. A token
// leaves when it expires. Brig mints one per route on every exec, so only a
// sandbox with more execs than half of this within a token's lifetime loses
// its oldest tokens early.
const maxLive = 1024

// anchorLen is the length of the end of each spelling the scan hashes. A
// spelling's end encodes the token's MAC, which differs between tokens, so
// an anchor hit is nearly always a real one.
const anchorLen = 32

// hashBase is the multiplier of the rolling hash.
const hashBase = 0x100000001b3

// anchorBits is the number of top hash bits that index the anchor filter.
const anchorBits = 20

// scanChunk is how much of a body the scan reads at a time.
const scanChunk = 64 << 10

// liveToken is a session token minted for, or presented by, a sandbox.
type liveToken struct {
	token, route string
	exp          time.Time
}

// form is a way of writing a token. The base64 form also has an alignment.
type form uint8

const (
	formRaw form = iota
	formPercentUpper
	formPercentLower
	formJSONUpper
	formJSONLower
	formHexLower
	formHexUpper
	formBase64
)

const (
	upperHex = "0123456789ABCDEF"
	lowerHex = "0123456789abcdef"
)

// base64Alphabet is the standard alphabet. A token is letters, digits, -, _
// and ., and no group of its bytes encodes to a 62 or a 63, the two values
// where the URL alphabet differs. So a token's base64 is the same in both,
// and holds neither / nor +.
const base64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64Skip is, per alignment, the number of leading characters of the
// encoding that also encode the bytes before the token.
var base64Skip = [3]int{0, 2, 3}

// spelling is one way the scan looks for one token. It holds no bytes: each
// byte is computed from the token, so a sandbox's tokens cost the same
// memory in every spelling.
//
// The spellings of a token are: raw, every byte percent-escaped or
// \u-escaped in upper or lower case hex, hex in either case, and base64 at
// each of the three alignments it can start at within a longer encoded
// string. A base64 spelling leaves out the
// characters that also encode the bytes around the token, so it is found
// with or without padding, alone or inside the encoding of a longer string,
// such as "Bearer <token>". None holds white space or a backslash escape
// that the scan drops.
type spelling struct {
	tok   int32
	form  form
	align uint8
}

// spellingsOf returns every spelling of the token at index tok.
func spellingsOf(tok int32) []spelling {
	out := []spelling{
		{tok: tok, form: formRaw}, {tok: tok, form: formPercentUpper}, {tok: tok, form: formPercentLower},
		{tok: tok, form: formJSONUpper}, {tok: tok, form: formJSONLower},
		{tok: tok, form: formHexLower}, {tok: tok, form: formHexUpper},
	}
	for align := range uint8(3) {
		out = append(out, spelling{tok: tok, form: formBase64, align: align})
	}
	return out
}

func (s spelling) encoding() string {
	switch s.form {
	case formRaw:
		return "raw"
	case formPercentUpper, formPercentLower:
		return "percent"
	case formJSONUpper, formJSONLower:
		return "json"
	case formHexLower, formHexUpper:
		return "hex"
	}
	return "base64"
}

// length returns the length of the spelling of tok.
func (s spelling) length(tok string) int {
	n := len(tok)
	switch s.form {
	case formRaw:
		return n
	case formPercentUpper, formPercentLower:
		return 3 * n
	case formJSONUpper, formJSONLower:
		return 6 * n
	case formHexLower, formHexUpper:
		return 2 * n
	}
	l := int(s.align) + n
	full := (l*8 + 5) / 6
	if l%3 != 0 {
		// The last character also encodes the bits after the token.
		full--
	}
	return full - base64Skip[s.align]
}

// at returns byte i of the spelling of tok.
func (s spelling) at(tok string, i int) byte {
	switch s.form {
	case formRaw:
		return tok[i]
	case formPercentUpper, formPercentLower:
		digits := upperHex
		if s.form == formPercentLower {
			digits = lowerHex
		}
		c := tok[i/3]
		switch i % 3 {
		case 0:
			return '%'
		case 1:
			return digits[c>>4]
		}
		return digits[c&15]
	case formJSONUpper, formJSONLower:
		digits := upperHex
		if s.form == formJSONLower {
			digits = lowerHex
		}
		c := tok[i/6]
		switch i % 6 {
		case 0:
			return '\\'
		case 1:
			return 'u'
		case 2, 3:
			return '0'
		case 4:
			return digits[c>>4]
		}
		return digits[c&15]
	case formHexLower, formHexUpper:
		digits := upperHex
		if s.form == formHexLower {
			digits = lowerHex
		}
		if i%2 == 0 {
			return digits[tok[i/2]>>4]
		}
		return digits[tok[i/2]&15]
	}
	// The encoding of the token after align zero bytes, from character
	// base64Skip[align] on.
	j := i + base64Skip[s.align]
	g := j/4*3 - int(s.align)
	b := func(x int) byte {
		if x < 0 || x >= len(tok) {
			return 0
		}
		return tok[x]
	}
	var v byte
	switch j % 4 {
	case 0:
		v = b(g) >> 2
	case 1:
		v = b(g)&3<<4 | b(g+1)>>4
	case 2:
		v = b(g+1)&15<<2 | b(g+2)>>6
	default:
		v = b(g+2) & 63
	}
	return base64Alphabet[v]
}

// anchored is a spelling with the hash of its last anchorLen bytes.
type anchored struct {
	hash uint64
	spelling
}

// matcher finds any spelling of a set of tokens in a byte string, in one
// pass over it. It hashes every window of anchorLen bytes, and compares a
// spelling only where its last anchorLen bytes hash the same. It is built
// once per change of the set, and is read-only after.
type matcher struct {
	tokens []liveToken
	// anchors are sorted by hash.
	anchors []anchored
	filter  []uint64
	pow     uint64
	// longest is the length of the longest spelling. A scan that goes on in
	// the next chunk keeps that many bytes less one.
	longest int
}

// remember adds tok to the sandbox's live tokens, or moves it to the end if
// it is there, so a token in use is the last to be dropped. The matcher is
// built again only when the set changes.
func (sb *sandbox) remember(tok, route string, exp time.Time) {
	sb.liveMu.Lock()
	defer sb.liveMu.Unlock()
	if i := slices.IndexFunc(sb.live, func(l liveToken) bool { return l.token == tok }); i >= 0 {
		l := sb.live[i]
		sb.live = append(slices.Delete(sb.live, i, i+1), l)
		return
	}
	sb.live = append(sb.live, liveToken{token: tok, route: route, exp: exp})
	if len(sb.live) > maxLive {
		sb.live = slices.Delete(sb.live, 0, len(sb.live)-maxLive)
	}
	sb.match = nil
}

// matcher returns the matcher of the tokens of sb that have not expired at
// now.
func (sb *sandbox) matcher(now time.Time) *matcher {
	sb.liveMu.Lock()
	defer sb.liveMu.Unlock()
	kept := slices.DeleteFunc(sb.live, func(l liveToken) bool { return !now.Before(l.exp) })
	if len(kept) != len(sb.live) {
		sb.match = nil
	}
	sb.live = kept
	if sb.match == nil {
		sb.match = newMatcher(sb.live)
	}
	return sb.match
}

func newMatcher(live []liveToken) *matcher {
	m := &matcher{tokens: slices.Clone(live), filter: make([]uint64, 1<<anchorBits/64), pow: 1}
	for range anchorLen - 1 {
		m.pow *= hashBase
	}
	for i, l := range m.tokens {
		for _, s := range spellingsOf(int32(i)) {
			n := s.length(l.token)
			if n < anchorLen {
				continue
			}
			var h uint64
			for j := n - anchorLen; j < n; j++ {
				h = h*hashBase + uint64(s.at(l.token, j))
			}
			m.anchors = append(m.anchors, anchored{hash: h, spelling: s})
			bit := h >> (64 - anchorBits)
			m.filter[bit/64] |= 1 << (bit % 64)
			m.longest = max(m.longest, n)
		}
	}
	sort.Slice(m.anchors, func(i, j int) bool { return m.anchors[i].hash < m.anchors[j].hash })
	return m
}

// hit is a spelling found in a request.
type hit struct {
	route, encoding string
}

// find returns the first spelling that b holds, or nil.
func (m *matcher) find(b []byte) *hit {
	if len(b) < anchorLen || len(m.anchors) == 0 {
		return nil
	}
	var h uint64
	for _, c := range b[:anchorLen] {
		h = h*hashBase + uint64(c)
	}
	for end := anchorLen; ; end++ {
		// h is the hash of b[end-anchorLen:end].
		if bit := h >> (64 - anchorBits); m.filter[bit/64]&(1<<(bit%64)) != 0 {
			i := sort.Search(len(m.anchors), func(i int) bool { return m.anchors[i].hash >= h })
			for ; i < len(m.anchors) && m.anchors[i].hash == h; i++ {
				if s := m.anchors[i].spelling; m.endsWith(b[:end], s) {
					return &hit{route: m.tokens[s.tok].route, encoding: s.encoding()}
				}
			}
		}
		if end == len(b) {
			return nil
		}
		h = (h-uint64(b[end-anchorLen])*m.pow)*hashBase + uint64(b[end])
	}
}

// endsWith reports whether b ends with spelling s, compared from its first
// byte.
func (m *matcher) endsWith(b []byte, s spelling) bool {
	tok := m.tokens[s.tok].token
	n := s.length(tok)
	if n > len(b) {
		return false
	}
	b = b[len(b)-n:]
	for i := range n {
		if b[i] != s.at(tok, i) {
			return false
		}
	}
	return true
}

// scan looks for the spellings of a matcher in a stream. It drops white
// space and the JSON escapes \n, \r and \t, and reads \/ as /, each also
// escaped again, as \\n is in a JSON string inside a JSON string. So a token
// that line breaks split is found, also inside a JSON string. A spelling or
// an escape that spans two writes is found.
type scan struct {
	m    *matcher
	work []byte
	// slashes counts the backslashes at the end of what was read, not yet
	// added to work.
	slashes int
}

func (m *matcher) scan() *scan { return &scan{m: m} }

// write scans p, and returns the first spelling found so far, or nil.
func (s *scan) write(p []byte) *hit {
	for len(p) > 0 {
		n := min(len(p), scanChunk)
		s.squeeze(p[:n])
		p = p[n:]
		if hit := s.m.find(s.work); hit != nil {
			return hit
		}
		// A matcher with no live token has a longest of 0, and keeps
		// nothing.
		if keep := max(s.m.longest-1, 0); len(s.work) > keep {
			s.work = append(s.work[:0], s.work[len(s.work)-keep:]...)
		}
	}
	return nil
}

// squeeze appends p to the work buffer without white space and the escapes
// the scan drops. A run of backslashes is held until the byte after it shows
// whether it is one of them.
func (s *scan) squeeze(p []byte) {
	for _, c := range p {
		if c == '\\' {
			s.slashes++
			continue
		}
		if s.slashes > 0 {
			switch c {
			case 'n', 'r', 't':
				s.slashes = 0
				continue
			case '/':
				s.slashes = 0
				s.work = append(s.work, '/')
				continue
			}
			for ; s.slashes > 0; s.slashes-- {
				s.work = append(s.work, '\\')
			}
		}
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			s.work = append(s.work, c)
		}
	}
}

// findString scans each string on its own.
func (m *matcher) findString(strs ...string) *hit {
	for _, str := range strs {
		if hit := m.scan().write([]byte(str)); hit != nil {
			return hit
		}
	}
	return nil
}

// placeholder is a session token found outside its carrier.
type placeholder struct {
	// route is the route of the token found.
	route string
	// where is the part of the request it was in: method, path, query,
	// header <name>, header name, trailer <name> or body.
	where string
	// encoding is the spelling it was found in, such as base64, with gzip+
	// in front when it was in a compressed body.
	encoding string
}

var (
	// errContentEncoding is a request body in a compression the scan cannot
	// read, or one that does not decompress.
	errContentEncoding = errors.New("content encoding")
	// errInflated is a compressed body read whole that decompresses past
	// maxReplay, so the scan cannot read all of it.
	errInflated = errors.New("the request body decompresses past the cap")
	// errBudget is a body the replay budget could not hold in time.
	errBudget = errors.New("the replay budget is spent")
)

// maxWhere is the longest header name a refusal and the audit line carry.
const maxWhere = 64

// findPlaceholder looks for any live token of sb outside the carrier of the
// request: in the method, the path, the query, the Host, every header name
// and value, the held body, and the trailers of a body read whole. The
// carriers are the positions the route reads a token from that present tok,
// the token the request was verified with. What else a carrier holds is
// scanned too.
//
// partial reports that only the start of a body above the cap was scanned.
func findPlaceholder(m *matcher, r *http.Request, p routePath, in *v1alpha1.Inject, tok string,
	held *heldBody) (_ *placeholder, partial bool, _ error) {
	at := func(where string, h *hit) *placeholder {
		return &placeholder{route: h.route, where: where, encoding: h.encoding}
	}
	if h := m.findString(r.Method); h != nil {
		return at("method", h), false, nil
	}
	if h := m.findString(p.rest, p.escapedRest); h != nil {
		return at("path", h), false, nil
	}
	if h := m.findString(r.URL.RawQuery, unescapeLoose(r.URL.RawQuery)); h != nil {
		return at("query", h), false, nil
	}
	if h := m.findString(r.Host); h != nil {
		return at("header Host", h), false, nil
	}
	if ph := findInHeader(m, r.Header, "header", carrierRests(in, r.Header, tok)); ph != nil {
		return ph, false, nil
	}
	if held == nil {
		return nil, false, nil
	}
	h, partial, err := findInBody(m, r.Header, held)
	if h != nil {
		return at("body", h), partial, nil
	}
	if err != nil {
		return nil, partial, err
	}
	if held.whole {
		if ph := findInHeader(m, r.Trailer, "trailer", nil); ph != nil {
			return ph, partial, nil
		}
	}
	return nil, partial, nil
}

// unescapeLoose decodes the percent escapes of a query and reads + as a
// space. An escape that is not two hex digits stays as it is, so one bad
// pair does not keep the rest from being decoded.
func unescapeLoose(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	unhex := func(c byte) (byte, bool) {
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
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '+':
			b = append(b, ' ')
		case c == '%' && i+2 < len(s):
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b = append(b, hi<<4|lo)
				i += 2
				continue
			}
			b = append(b, c)
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// findInHeader scans every name and value of h, in name order. rests holds,
// per carrier, what is left of its first value without the token.
func findInHeader(m *matcher, h http.Header, kind string, rests map[string]string) *placeholder {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	// A name that holds a token is not repeated on the audit line.
	if hit := m.findString(names...); hit != nil {
		return &placeholder{route: hit.route, where: kind + " name", encoding: hit.encoding}
	}
	for _, name := range names {
		values := h[name]
		if rest, ok := rests[name]; ok && len(values) > 0 {
			values = append([]string{rest}, values[1:]...)
		}
		if hit := m.findString(values...); hit != nil {
			return &placeholder{route: hit.route, where: kind + " " + clip(name, maxWhere), encoding: hit.encoding}
		}
	}
	return nil
}

// carrierRests returns the headers that carry tok where the route reads a
// token, each with what its first value holds besides tok: the scheme of a
// bearer value, the username of a basic one, and the prefix and suffix of a
// header the route sets. A tool can send the same token in two carriers of
// one route. Another token, or the same one twice, is not taken out.
func carrierRests(in *v1alpha1.Inject, h http.Header, tok string) map[string]string {
	rests := map[string]string{}
	auth := h.Get("Authorization")
	if scheme, t, ok := strings.Cut(auth, " "); ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(t) == tok {
		rests["Authorization"] = strings.Replace(auth, tok, "", 1)
	} else if in.BasicAuth != nil {
		r := http.Request{Header: http.Header{"Authorization": {auth}}}
		if user, pass, ok := r.BasicAuth(); ok && pass == tok {
			rests["Authorization"] = user
		}
	}
	if in.Headers != nil {
		for _, s := range in.Headers.Set {
			name := http.CanonicalHeaderKey(s.Name)
			prefix, suffix, ok := strings.Cut(s.Value, v1alpha1.PlaceholderValue)
			if !ok || name == "Authorization" {
				continue
			}
			if h.Get(name) == prefix+tok+suffix {
				rests[name] = prefix + suffix
			}
		}
	}
	return rests
}

// findInBody scans the held body. A body in gzip or deflate is scanned as
// it decompresses, up to maxReplay bytes, and then as sent: the upstream
// gets the bytes as sent, and one that ignores the coding reads them as
// they are. Any other content coding is refused, since the scan cannot read
// it.
func findInBody(m *matcher, h http.Header, held *heldBody) (*hit, bool, error) {
	var codings []string
	for _, v := range h.Values("Content-Encoding") {
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	if len(codings) == 0 {
		return m.scan().write(held.buf), !held.whole, nil
	}
	hit, partial, err := findDecompressed(m, codings, held)
	if hit == nil {
		if raw := m.scan().write(held.buf); raw != nil {
			return raw, !held.whole, nil
		}
	}
	return hit, partial, err
}

// findDecompressed scans the held body as it decompresses.
func findDecompressed(m *matcher, codings []string, held *heldBody) (*hit, bool, error) {
	partial := !held.whole
	if len(codings) > 1 {
		return nil, partial, errContentEncoding
	}
	br := bytes.NewReader(held.buf)
	// A compressed body cut at the cap ends early, there and nowhere else.
	bad := func(err error) (*hit, bool, error) {
		if partial && errors.Is(err, io.ErrUnexpectedEOF) && br.Len() == 0 {
			return nil, partial, nil
		}
		return nil, partial, errContentEncoding
	}
	coding := strings.TrimPrefix(codings[0], "x-")
	var rd io.Reader
	switch coding {
	case "gzip":
		zr, err := gzip.NewReader(br)
		if err != nil {
			return bad(err)
		}
		if hit := m.findString(zr.Name, zr.Comment, string(zr.Extra)); hit != nil {
			return compressed(hit, coding), partial, nil
		}
		rd = zr
	case "deflate":
		// HTTP's deflate is zlib, and some clients send raw deflate.
		if zr, err := zlib.NewReader(br); err == nil {
			rd = zr
		} else {
			br.Reset(held.buf)
			rd = flate.NewReader(br)
		}
	default:
		return nil, partial, errContentEncoding
	}
	sc := m.scan()
	buf := make([]byte, scanChunk)
	lr := io.LimitReader(rd, maxReplay+1)
	var total int64
	for {
		n, err := lr.Read(buf)
		over := total+int64(n) > maxReplay
		if over {
			n = int(maxReplay - total)
		}
		total += int64(n)
		if hit := sc.write(buf[:n]); hit != nil {
			return compressed(hit, coding), partial || over, nil
		}
		switch {
		case over && held.whole:
			return nil, partial, errInflated
		case over:
			return nil, true, nil
		case err == io.EOF:
			// The decoders read no further than their stream, so bytes
			// left after it, or a body that goes on past the cap, were
			// never decompressed.
			if partial || br.Len() > 0 {
				return nil, partial, errContentEncoding
			}
			return nil, partial, nil
		case err != nil:
			return bad(err)
		}
	}
}

func compressed(h *hit, coding string) *hit {
	return &hit{route: h.route, encoding: coding + "+" + h.encoding}
}

// maxHeld is what holdBody reads at most: the cap, and one byte more to tell
// a body of exactly maxReplay bytes from a longer one.
const maxHeld = maxReplay + 1

// holdStep is the least the held buffer grows by, and the most a body takes
// from the budget before its first bytes arrive.
const holdStep = 32 << 10

// heldBody is the start of a request body, read before the request goes
// upstream so it can be scanned, and kept for a retry after an upstream 401.
// It holds at most maxReplay bytes and one more, taken from the Handler's
// replay budget as they arrive.
type heldBody struct {
	buf []byte
	// whole reports that buf is the whole body. A whole body of maxHeld
	// bytes is scanned whole and is still too large to send again.
	whole   bool
	taken   int64
	sandbox string
	budget  *budget
}

// holdBody reads the start of r's body and puts it back in front of the
// rest, so the upstream gets the body as the guest sent it. It returns nil
// for a request without a body.
//
// The buffer grows as bytes arrive, and each step is taken from the
// sandbox's share of the budget, so a body that declares a length and
// stalls holds one step. A step the budget cannot give within a tenth of
// bodyWait is errBudget, and the body is never forwarded unscanned. Reading
// the held part has a deadline of bodyWait, which stays set after an error,
// so the server does not wait for the rest. The rest of a longer body
// streams with no deadline.
func (h *Handler) holdBody(w http.ResponseWriter, r *http.Request, sandbox string) (*heldBody, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	start := time.Now()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(start.Add(h.bodyWait))
	hb := &heldBody{sandbox: sandbox, budget: h.replay}
	share := h.share()
	orig := r.Body
	for len(hb.buf) < maxHeld {
		if len(hb.buf) == cap(hb.buf) {
			grow := min(max(len(hb.buf)/2, holdStep), 1<<20, maxHeld-len(hb.buf))
			if len(hb.buf) == 0 && r.ContentLength >= 0 && r.ContentLength < int64(grow) {
				grow = int(r.ContentLength) + 1
			}
			if err := hb.take(r.Context(), int64(grow), share, start.Add(h.bodyWait/10)); err != nil {
				return hb, err
			}
			hb.buf = append(make([]byte, 0, cap(hb.buf)+grow), hb.buf...)
		}
		n, err := orig.Read(hb.buf[len(hb.buf):cap(hb.buf)])
		hb.buf = hb.buf[:len(hb.buf)+n]
		if err == io.EOF {
			hb.whole = true
			break
		}
		if err != nil {
			return hb, err
		}
	}
	_ = rc.SetReadDeadline(time.Time{})
	if hb.whole {
		r.Body = io.NopCloser(bytes.NewReader(hb.buf))
	} else {
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(hb.buf), orig), orig}
	}
	return hb, nil
}

// take takes n bytes from the sandbox's share, waiting for them until the
// deadline or the end of the request.
func (b *heldBody) take(ctx context.Context, n, share int64, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		ok, freed := b.budget.take(b.sandbox, n, share)
		if ok {
			b.taken += n
			return nil
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return errBudget
		case <-timer.C:
			return errBudget
		}
	}
}

// timedOut reports whether err is a read that passed its deadline.
func timedOut(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// complete returns the whole body when it is at most maxReplay bytes.
func (b *heldBody) complete() ([]byte, bool) {
	if !b.whole || len(b.buf) > maxReplay {
		return nil, false
	}
	return b.buf, true
}

// release gives the bytes back to the budget once the request is done.
func (b *heldBody) release() {
	if b == nil {
		return
	}
	b.budget.give(b.sandbox, b.taken)
	b.taken, b.buf = 0, nil
}

// Placeholders returns, per registered sandbox, the number of requests
// refused for a session token outside its carrier. A sandbox with none is
// left out.
func (h *Handler) Placeholders() map[string]int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := map[string]int64{}
	for id, sb := range h.sandboxes {
		if n := sb.placeholders.Load(); n > 0 {
			out[id] = n
		}
	}
	return out
}
