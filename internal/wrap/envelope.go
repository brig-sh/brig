package wrap

import (
	"fmt"
	"io"
	"strings"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/profile"
	"github.com/brig-sh/brig/internal/runtime"
)

// envelopeRow is one line of the execution envelope: a label and the value
// that sits under it. The value carries its own parenthetical detail, so the
// renderer only has to line the labels up.
type envelopeRow struct {
	label string
	value string
}

// envelope is the boundary a run is about to trust, in the order a reader
// meets it: whose session, which profile, the sandbox and the runtime under
// it, the directory that becomes the guest's home, the image, and the
// credentials handed in.
//
// It is read from the same Config the full report reads, and the credential
// row from the same creds.Set and resolved secrets the run itself forwards, so
// the block and `brig info` cannot come to disagree about what a sandbox
// received. That single source is the whole reason to route both through here
// rather than to gather the facts a second time.
//
// The rows are a slice rather than a fixed set of fields because more of them
// are coming: the isolation mechanism, the verify mode and the network posture
// each add a row, and a slice takes one without a caller learning a new field
// or the renderer changing. A new row appends here and nothing downstream
// moves.
func (c *Config) envelope(set creds.Set) []envelopeRow {
	var rows []envelopeRow
	// Only when named. An unnamed run has no session of its own to report, and
	// the profile and the sandbox rows below already carry everything a bare
	// run has to say.
	if c.RawName != "" {
		rows = append(rows, envelopeRow{"SESSION", c.RawName})
	}
	// The runtime is the one thing in the envelope that needs a runtime. brig
	// info and brig env answer without one, marking the line unavailable the
	// way the full report does, rather than failing the whole envelope: the
	// person reading it is often the one whose runtime is what broke.
	kind := "runtime unavailable"
	if c.Runtime != nil {
		kind = c.Runtime.Kind()
	}
	rows = append(rows,
		envelopeRow{"PROFILE", c.Profile.Name},
		envelopeRow{"SANDBOX", fmt.Sprintf("%s (%s)", c.VMName, kind)},
		// Directly under the sandbox, because it is the same subject: the row
		// above says which runtime holds the sandbox, this one says what that
		// actually gets you. The sentence brig's own documentation makes -- the
		// sandbox is a microVM on both operating systems -- is true of a default
		// install and not of every run, and this is the row that tells the two
		// apart for the person in front of it.
		envelopeRow{"ISOLATION", c.isolationLine()},
		// The workspace is mounted as the guest's home, read-write. The
		// annotation is here rather than implied so a later read-only mount has
		// a place to say so in the row it already has, instead of needing a new
		// one.
		envelopeRow{"WORKSPACE", fmt.Sprintf("%s (read-write)", c.Workspace)},
	)
	// Directly under the home, because it is the same subject read twice: two
	// host directories this run is about to hand a sandbox read-write. Only
	// when there is one -- a run that named no project has no second mount to
	// report, and a row saying so on every ordinary run is a row people learn
	// to skip. The guest path is named beside the host path because it is the
	// answer to the question the row raises, which is where the agent will be.
	if c.Project != "" {
		rows = append(rows, envelopeRow{"PROJECT",
			fmt.Sprintf("%s (read-write, mounted at %s)", c.Project, c.GuestProject)})
	}
	rows = append(rows,
		// The pull policy, the same detail the full report prints beside the
		// image. Whether the signature verified is a separate fact that the
		// verify-mode row will carry, so it is deliberately not folded in here.
		envelopeRow{"IMAGE", fmt.Sprintf("%s (pull %s)", c.Image, c.Pull)},
		envelopeRow{"CREDENTIALS", c.credentialLine(set)},
		// The posture, said out loud. It decides what the agent can reach, and
		// until it appeared here the only way to know was to remember which
		// setting the run was started with.
		envelopeRow{"NETWORK", c.Network.Line()},
	)
	return rows
}

// isolationLine is the ISOLATION row: what the sandbox this run would boot
// actually stands on.
//
// The runtime answers it, because the runtime is what knows -- the hypervisor
// under hull, the containerd shim under nerdctl or docker. brig only supplies
// the backend this run resolved to, so the row names the one that would boot
// rather than the one a profile happens to carry.
//
// No runtime is the one case brig answers itself, and it answers "cannot tell".
// The alternative is to print what a working install would have given, which is
// the documentation's claim rather than this host's, and this row is worth
// having only if it never makes that claim on evidence it does not have.
func (c *Config) isolationLine() string {
	if c.Runtime == nil {
		return string(runtime.BoundaryUnknown) +
			" (no runtime, so brig cannot tell what a sandbox here would stand on)"
	}
	return c.Runtime.Isolation(c.hypervisor()).Line()
}

// credentialLine is the CREDENTIALS row: every credential this run hands the
// guest, by name, and never a value.
//
// A credential reaches the guest one of two ways, and the row names both so the
// login surface is visible in one place:
//   - as an environment variable, which creds.Set already tracks by name; and
//   - as a file written into a tmpfs, which the profile's files: bindings
//     declare and the resolved store fills.
//
// The file half is read from the same structures deliverSecretFiles writes
// from -- the files: bindings and the resolved secret values -- so the row
// names exactly what will be delivered rather than a separate guess at it. Only
// the presence of a value is read, never the value itself.
func (c *Config) credentialLine(set creds.Set) string {
	var names []string
	for _, n := range credentialNames(set) {
		names = append(names, bareName(n))
	}
	for _, b := range c.Profile.Files {
		// A ref this cannot read names no credential, so the row leaves it out
		// rather than printing something the reader cannot act on. Skipping is
		// safe in both directions: Validate refuses a malformed ref at load, for
		// a file profile and a built-in alike, so one does not reach here; and
		// if one ever did, writeSecretFiles fails the run on the same ref. The
		// block under-reports and the run then stops, which is the right way
		// round -- the opposite, naming a file as delivered when the binding
		// naming it is unreadable, is the claim that would be false.
		r, err := profile.ParseRef(b.Ref)
		if err != nil {
			continue
		}
		if _, ok := c.secrets.Values[r.Name]; ok {
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// bareName drops the source annotation the full report carries -- "TOK(secret)"
// becomes "TOK" -- because the block names credentials plainly and the detailed
// report is where the source belongs.
func bareName(s string) string {
	if i := strings.LastIndex(s, "("); i > 0 && strings.HasSuffix(s, ")") {
		return strings.TrimRight(s[:i], " ")
	}
	return s
}

// renderEnvelope writes the block to w, labels left-aligned to a common width
// so the values form a column. The width is measured rather than fixed so a
// longer label a future row introduces widens the column instead of breaking
// the alignment.
func (c *Config) renderEnvelope(w io.Writer, set creds.Set) {
	rows := c.envelope(set)
	width := 0
	for _, r := range rows {
		if len(r.label) > width {
			width = len(r.label)
		}
	}
	for _, r := range rows {
		fmt.Fprintf(w, "%-*s  %s\n", width, r.label, r.value)
	}
}

// PrintPreRunEnvelope writes the envelope before a run or create boots the
// sandbox, to stderr. Stderr rather than stdout so it never lands in the
// agent's own output or in the sandbox name a scripted create captures; the
// block is a notice, not the command's result. --quiet suppresses it entirely.
func (c *Config) PrintPreRunEnvelope(set creds.Set) { c.renderEnvelope(c.Err, set) }

// Info is `brig info`: the envelope, then everything `brig env` has always
// printed, both to stdout because here the report is the point. `brig env` is
// kept as a deprecated spelling that calls this and prints one line naming the
// new one.
func (c *Config) Info(set creds.Set) {
	c.renderEnvelope(c.Out, set)
	c.Status(set)
}
