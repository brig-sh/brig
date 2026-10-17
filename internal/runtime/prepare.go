package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A move used to carry the whole guest while it was paused: its memory and
// every block of its disk that held data, almost all of it the image, which
// the destination could have had before the move began. `hull migrate-prepare`
// puts what does not change on the destination ahead of time, while the guest
// runs: the sandbox's base disk (the disk as hull built it, named by its
// digest), the portable kernel and the image. A streamed move then carries
// only what the guest changed. Each step finds its effect already there and
// skips, so a prepare can be run any number of times.
//
// It starts no network gateway. The moved guest's gateway, with its egress
// rules, is brig's to start on the destination at the move
// (teleport-receive), and a gateway hull started ahead of it would have none.
//
// Optional, like Migrator: a runtime that cannot move a sandbox has nothing
// to prepare.

// PrepareSpec is a request to make a host ready to take a sandbox.
type PrepareSpec struct {
	// RemoteStore and RemoteHull are the hull store and the hull the brig
	// on the destination drives, as for MigrateSpec: a base copied into
	// another store is one the move does not find.
	RemoteStore string
	RemoteHull  string
	// Progress, when set, is handed each line hull prints on stderr, as
	// hull prints it. As for MigrateSpec, it must not block.
	Progress func(line string)
}

// Prepared is hull's report of a prepare: what it did for each part, and
// what the destination then holds.
type Prepared struct {
	// Steps are what hull did for each part, in its order.
	Steps []PrepareStep `json:"steps,omitempty"`
	// Base is the digest of the sandbox's base disk, when the destination
	// holds it.
	Base string `json:"base,omitempty"`
	// Kernels are the profile kernels the destination holds, by profile,
	// each by its sha256.
	Kernels map[string]string `json:"kernels,omitempty"`
	// Stream reports whether a move there sends only what the guest
	// changed. Why says why not, when hull said.
	Stream bool   `json:"stream"`
	Why    string `json:"why,omitempty"`
}

// PrepareStep is one part of a prepare: what (`image 1a2b...`, `base
// 3c4d...`, `portable-arm64-v1 kernel`), and its result: held, sent,
// skipped or failed.
type PrepareStep struct {
	What    string  `json:"what"`
	Result  string  `json:"result"`
	Detail  string  `json:"detail,omitempty"`
	Bytes   int64   `json:"bytes,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// MovePreparer makes a host ready to take a sandbox before it moves there.
type MovePreparer interface {
	// MigratePrepare makes the host named as USER@HOST ready to take the
	// sandbox, and says what it then holds. It pauses nothing.
	MigratePrepare(name, to string, spec PrepareSpec) (Prepared, error)
}

var _ MovePreparer = (*hull)(nil)

// MigratePrepare runs `hull migrate-prepare --json`: its lines on stderr go
// to Progress as they come, and its report is the JSON on stdout. It reaches
// the destination over ssh to the host given, as a move does, so it is not
// handed another transport either.
func (h *hull) MigratePrepare(name, to string, spec PrepareSpec) (Prepared, error) {
	pin := ""
	if h.prepareTakesKernelPin() {
		pin = PortableKernelDigest
	}
	args, err := prepareArgs(name, to, pin, spec)
	if err != nil {
		return Prepared{}, err
	}
	out, err := h.callLines(args, spec.Progress, moveTransports...)
	if err != nil {
		return Prepared{}, err
	}
	return preparedOf(name, to, out)
}

// prepareTakesKernelPin reports whether this hull's prepare takes
// --kernel-sha256, the portable kernel the destination is to hold. With it,
// a prepare to a host that holds another portable kernel, such as one set up
// before brig pinned this one, replaces that kernel with the pinned one when
// this host's is the pin; brig refuses to boot any other there. A hull from
// before the flag would reject it, so it is asked first, as takesProfile
// asks; a hull that does not answer gets the prepare without it, which
// leaves another kernel there as it is. The answer is kept on h, since it
// cannot change while brig runs.
func (h *hull) prepareTakesKernelPin() bool {
	h.kernelPinOnce.Do(func() {
		out, err := h.help("migrate-prepare")
		h.kernelPin = err == nil && flagListed(out, "--kernel-sha256")
	})
	return h.kernelPin
}

// prepareArgs is the one place the prepare's command line is built, flags
// first and the sandbox last, as for a move. The host is checked here as
// well, since it goes to hull and from hull to ssh. pin, when not "", is the
// portable kernel's digest for --kernel-sha256.
func prepareArgs(name, to, pin string, spec PrepareSpec) ([]string, error) {
	if err := CheckRemote(to); err != nil {
		return nil, err
	}
	if name == "" || strings.HasPrefix(name, "-") {
		return nil, fmt.Errorf("%q is not a sandbox name hull takes", name)
	}
	args := []string{"migrate-prepare", "--to", to}
	for _, f := range []struct{ flag, value string }{
		{"--remote-store", spec.RemoteStore},
		{"--remote-hull", spec.RemoteHull},
		{"--kernel-sha256", pin},
	} {
		if f.value != "" {
			args = append(args, f.flag, f.value)
		}
	}
	return append(args, "--json", name), nil
}

// prepareReport is the part of `hull migrate-prepare --json` brig reads.
type prepareReport struct {
	Name  string        `json:"name"`
	To    string        `json:"to"`
	Steps []PrepareStep `json:"steps"`
	Holds *struct {
		Bases          []string          `json:"bases"`
		ProfileKernels map[string]string `json:"profileKernels"`
	} `json:"holds"`
	Stream bool   `json:"stream"`
	Why    string `json:"why"`
}

// preparedOf reads hull's report of a prepare of name on to. A report for
// another sandbox or host, or one that does not read, is an error rather
// than a guess at what the host holds.
func preparedOf(name, to, out string) (Prepared, error) {
	var r prepareReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil {
		return Prepared{}, fmt.Errorf("hull's report of the prepare did not read: %w", err)
	}
	if r.Name != name || r.To != to {
		return Prepared{}, fmt.Errorf("hull reported a prepare of %s on %s, not of %s on %s", r.Name, r.To, name, to)
	}
	p := Prepared{Steps: r.Steps, Stream: r.Stream, Why: r.Why}
	if r.Holds != nil {
		p.Kernels = r.Holds.ProfileKernels
		// hull names the base in its step by the head of its digest; the
		// destination's list of bases has the whole of it.
		for _, st := range r.Steps {
			head, ok := strings.CutPrefix(st.What, "base ")
			if !ok || head == "" || (st.Result != "held" && st.Result != "sent") {
				continue
			}
			for _, b := range r.Holds.Bases {
				if strings.HasPrefix(b, head) {
					p.Base = b
				}
			}
		}
	}
	return p, nil
}
