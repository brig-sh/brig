package controlroom

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Limits of R21 and of the lines worth sending.
const (
	maxBatchEvents = 200
	maxBatchBytes  = 1 << 20
	maxLineBytes   = 64 << 10
)

// A Registry names the sandboxes a node runs.
type Registry interface {
	// Running returns the containers running now, each with the name of the
	// sandbox it runs: the name a recording of it carries.
	Running() map[string]string
}

// Reporter reports a node's sandboxes and the records its Sources pick from
// the capture directory.
type Reporter struct {
	Client   *Client
	Dir      string // the capture directory
	Registry Registry
	Sources  []Source
	// Runtime is reported with each sandbox, e.g. "hull" or "kubernetes".
	Runtime string
	// State is a file that keeps, across restarts, how far each file was sent
	// and which sandboxes were last reported running. Without one, a restart
	// sends what the Control Room already has, and it drops the duplicates.
	State string
	Log   *log.Logger

	mu      sync.Mutex
	offsets map[string]int64  // file name -> offset of the first unsent byte
	seen    map[string]string // container id -> sandbox name, last reported running
	names   map[string]string // container id -> sandbox name, ever seen, for the records it still has
	scanned map[string]bool   // source kind -> scanned once in this process
	refused map[string]bool   // kind -> the deployment does not take it
	probed  map[string]bool   // kind -> its first event of this run went alone
	stats   Stats
	backoff time.Duration
	next    time.Time
}

// Stats is what the reporter did.
type Stats struct {
	URL        string    `json:"url"`
	Sandboxes  int       `json:"sandboxes_reported"`
	Sent       int       `json:"events_sent"`
	Accepted   int       `json:"events_accepted"`
	Duplicates int       `json:"events_duplicate"`
	Rejected   int       `json:"events_rejected"`
	Refused    []string  `json:"kinds_refused,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	LastOK     time.Time `json:"last_ok,omitempty"`
}

// Stats returns a copy of the reporter's counters.
func (r *Reporter) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	s.URL = r.Client.URL()
	s.Refused = nil
	for k := range r.refused {
		s.Refused = append(s.Refused, k)
	}
	sort.Strings(s.Refused)
	return s
}

func (r *Reporter) logf(format string, a ...any) {
	if r.Log != nil {
		r.Log.Printf(format, a...)
	} else {
		log.Printf(format, a...)
	}
}

func (r *Reporter) init() {
	if r.offsets == nil {
		r.offsets = map[string]int64{}
	}
	if r.seen == nil {
		r.seen = map[string]string{}
	}
	if r.names == nil {
		r.names = map[string]string{}
	}
	if r.scanned == nil {
		r.scanned = map[string]bool{}
	}
	if r.refused == nil {
		r.refused = map[string]bool{}
	}
	if r.probed == nil {
		r.probed = map[string]bool{}
	}
}

// Run loads State and reports every interval until ctx ends.
func (r *Reporter) Run(ctx context.Context, interval time.Duration) {
	r.Load()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick reports once, unless a failure is still being backed off.
func (r *Reporter) Tick(ctx context.Context, now time.Time) {
	r.mu.Lock()
	r.init()
	wait := now.Before(r.next)
	r.mu.Unlock()
	if wait {
		return
	}
	err := r.report(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.backoff, r.next = 0, time.Time{}
		r.stats.LastError, r.stats.LastOK = "", now
		return
	}
	r.stats.LastError = err.Error()
	var se *StatusError
	switch {
	case errors.As(err, &se) && se.Status == 429:
		// The limit refills, so start over from the shortest wait.
		r.backoff = 2 * time.Second
	case errors.As(err, &se) && (se.Status == 401 || se.Status == 403):
		// A wrong or revoked credential does not heal on its own.
		r.backoff = 10 * time.Minute
	case r.backoff == 0:
		r.backoff = 2 * time.Second
	default:
		r.backoff *= 2
		if r.backoff > 60*time.Second {
			r.backoff = 60 * time.Second
		}
	}
	r.next = now.Add(r.backoff)
	r.logf("controlroom: %v; next try in %s", err, r.backoff)
}

// report sends the registry, then each source. A sandbox has to be in the
// node's registry before its events can name it.
func (r *Reporter) report(ctx context.Context) error {
	if err := r.reportSandboxes(ctx); err != nil {
		return err
	}
	for _, src := range r.Sources {
		r.mu.Lock()
		refused := r.refused[src.Kind]
		r.mu.Unlock()
		if refused {
			continue
		}
		if err := r.reportSource(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

// reportSandboxes sends the sandboxes that started or stopped since the last
// report. It sends no snapshot, so it leaves the node's other reporters'
// sandboxes alone.
func (r *Reporter) reportSandboxes(ctx context.Context) error {
	running := map[string]string{}
	for id, name := range r.Registry.Running() {
		if ValidName(name) {
			running[id] = name
		}
	}
	names := map[string]bool{}
	for _, name := range running {
		names[name] = true
	}
	r.mu.Lock()
	var sb []Sandbox
	for id, name := range running {
		if r.seen[id] != name {
			sb = append(sb, Sandbox{Sandbox: name, State: "running", Runtime: r.Runtime})
		}
	}
	for id, name := range r.seen {
		// A new container under the same name is the sandbox running, so
		// the old container's end does not stop it.
		if _, ok := running[id]; !ok && !names[name] {
			sb = append(sb, Sandbox{Sandbox: name, State: "stopped", Runtime: r.Runtime})
		}
	}
	r.mu.Unlock()
	if len(sb) > 0 {
		sort.Slice(sb, func(i, j int) bool { return sb[i].Sandbox < sb[j].Sandbox })
		if err := r.Client.ReportSandboxes(ctx, sb); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.stats.Sandboxes += len(sb)
	r.seen = running
	for id, name := range running {
		r.names[id] = name
	}
	r.mu.Unlock()
	if len(sb) > 0 {
		r.Save()
	}
	return nil
}

// pending is one event with where its line ends in its file.
type pending struct {
	ev  Event
	end int64
}

// reportSource sends what the source's files gained since the last report, a
// batch at a time, and moves a file's offset only past what the Control Room
// answered for.
func (r *Reporter) reportSource(ctx context.Context, src Source) error {
	ents, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	first := !r.scanned[src.Kind]
	r.scanned[src.Kind] = true
	r.mu.Unlock()
	var files []string
	for _, e := range ents {
		if _, ok := src.match(e.Name()); ok && e.Type().IsRegular() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		if first && src.FromEnd {
			r.startAtEnd(name)
		}
		budget := src.MaxScan
		for {
			batch, scanned, err := r.read(src, name, budget)
			if err != nil || len(batch) == 0 {
				break
			}
			if src.MaxScan > 0 {
				budget -= scanned
			}
			r.mu.Lock()
			if !r.probed[src.Kind] {
				// A kind's first event of a run goes with only the events of
				// its own line: if the deployment does not know the kind, a
				// handful are stored as unparsed, not 200.
				n := 1
				for n < len(batch) && batch[n].end == batch[0].end {
					n++
				}
				batch = batch[:n]
			}
			r.mu.Unlock()
			evs := make([]Event, len(batch))
			for i, p := range batch {
				evs[i] = p.ev
			}
			res, err := r.Client.IngestEvents(ctx, evs)
			if err != nil {
				return err
			}
			r.mu.Lock()
			r.probed[src.Kind] = true
			r.stats.Sent += len(evs)
			r.stats.Accepted += res.Accepted
			r.stats.Duplicates += res.Duplicates
			r.stats.Rejected += res.Rejected
			if res.Rejected == len(evs) && res.Accepted == 0 && res.Duplicates == 0 {
				// None stored as its kind: this deployment does not take the
				// kind. The offset stays, so nothing is lost when it does.
				r.refused[src.Kind] = true
				r.mu.Unlock()
				r.logf("controlroom: %s does not take %q events yet; they are kept and not sent", r.Client.URL(), src.Kind)
				return nil
			}
			r.offsets[name] = batch[len(batch)-1].end
			r.mu.Unlock()
			r.Save()
			if src.MaxScan > 0 && budget <= 0 {
				break
			}
		}
	}
	return nil
}

// startAtEnd sets a file with no offset yet to its current end.
func (r *Reporter) startAtEnd(name string) {
	fi, err := os.Stat(filepath.Join(r.Dir, name))
	if err != nil {
		return
	}
	r.mu.Lock()
	if _, ok := r.offsets[name]; !ok {
		r.offsets[name] = fi.Size()
	}
	r.mu.Unlock()
}

// read returns up to one batch of events from a file past its offset, and how
// many bytes it scanned. A file shorter than its offset starts over; a line
// still being written waits.
func (r *Reporter) read(src Source, name string, budget int64) ([]pending, int64, error) {
	path := filepath.Join(r.Dir, name)
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	id, _ := src.match(name)
	r.mu.Lock()
	off := r.offsets[name]
	if off > fi.Size() {
		off = 0
		r.offsets[name] = 0
	}
	sandbox := r.names[id]
	r.mu.Unlock()
	if off == fi.Size() {
		return nil, 0, nil
	}
	if sandbox == "" {
		sandbox = r.Registry.Running()[id]
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, 0, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var out []pending
	size, pos := 0, off
	for len(out) < maxBatchEvents {
		if src.MaxScan > 0 && pos-off >= budget {
			break
		}
		line, err := br.ReadString('\n')
		if err != nil {
			// No newline yet: the writer is still writing this line.
			break
		}
		start := pos
		pos += int64(len(line))
		text := line[:len(line)-1]
		if n := len(text); n > 0 && text[n-1] == '\r' {
			text = text[:n-1]
		}
		// The line sent is the source's, which may be shorter than the one
		// read (a tool call's arguments are capped), so the size that counts
		// is the event's.
		var evs []Event
		if text != "" {
			if ev, ok := src.Parse(text, id, sandbox); ok && len(ev.Line) <= maxLineBytes {
				ev.Kind, ev.File, ev.Offset = src.Kind, name, start
				evs = append(evs, ev)
			}
		}
		if len(evs) == 0 {
			// Nothing to send from this line: carry the offset past it with
			// the next event sent, or now when none is pending.
			if len(out) > 0 {
				out[len(out)-1].end = pos
			} else {
				r.mu.Lock()
				r.offsets[name] = pos
				r.mu.Unlock()
			}
			continue
		}
		n := 0
		for _, ev := range evs {
			n += len(ev.Line) + 256
		}
		if size+n > maxBatchBytes && len(out) > 0 {
			pos = start
			break
		}
		size += n
		for _, ev := range evs {
			out = append(out, pending{ev: ev, end: pos})
		}
	}
	return out, pos - off, nil
}

// ValidName reports whether s is a name the Control Room's registry takes for
// a sandbox.
func ValidName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if i > 0 {
			ok = ok || c == '.' || c == '_' || c == '-'
		}
		if !ok {
			return false
		}
	}
	return true
}

// state is what State keeps.
type state struct {
	Offsets map[string]int64  `json:"offsets"`
	Running map[string]string `json:"running"`
	Names   map[string]string `json:"names"`
}

// Load reads State. Run calls it; a caller that drives Tick itself calls it
// first.
func (r *Reporter) Load() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.init()
	if r.State == "" {
		return
	}
	b, err := os.ReadFile(r.State)
	if err != nil {
		return
	}
	var st state
	if json.Unmarshal(b, &st) != nil {
		return
	}
	for k, v := range st.Offsets {
		if v >= 0 {
			r.offsets[k] = v
		}
	}
	for id, name := range st.Running {
		if ValidName(name) {
			r.seen[id] = name
			r.names[id] = name
		}
	}
	for id, name := range st.Names {
		if ValidName(name) {
			r.names[id] = name
		}
	}
}

// Save writes State whole: a crash leaves the old file or the new one, not
// half of either.
func (r *Reporter) Save() {
	if r.State == "" {
		return
	}
	r.mu.Lock()
	// A name is kept while its container still has a file here.
	names := map[string]string{}
	for id, name := range r.names {
		for file := range r.offsets {
			if len(file) > len(id) && file[:len(id)] == id && file[len(id)] == '.' {
				names[id] = name
				break
			}
		}
	}
	b, err := json.Marshal(state{Offsets: r.offsets, Running: r.seen, Names: names})
	r.mu.Unlock()
	if err != nil {
		return
	}
	tmp := r.State + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		r.logf("controlroom: %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, r.State); err != nil {
		r.logf("controlroom: %s: %v", r.State, err)
	}
}
