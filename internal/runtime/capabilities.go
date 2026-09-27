package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// NestedSupport is what a runtime says about giving a guest hardware
// virtualization of its own. Detail is the runtime's own account, or the
// reason brig could not get one: a hull that timed out, or one whose answer
// brig cannot read, comes back as not supported with that reason here.
//
// Outdated marks the one reason that is not about the host: a hull released
// before nested virtualization, which has no `capabilities` command. Detail
// then says to upgrade it, and a report must not call the host unable.
type NestedSupport struct {
	Supported bool
	Outdated  bool
	Backend   string
	Detail    string
}

// CapabilityProber is a runtime that can say whether it can give a guest
// nested virtualization. A boot does not ask it; the note in supports says
// why.
//
// Optional: the Linux runtime does not pass /dev/kvm through at all and has
// nothing to report, and a stub answering "no" would be a claim about a
// question nobody put to it.
type CapabilityProber interface {
	NestedVirt() NestedSupport
}

// capabilitiesTimeout bounds `hull capabilities --json`. Longer than the 5s
// hull gives its own `hvi caps`, so a slow hvi comes back as hull's detail
// before brig gives up. A var so a test can shorten it.
var capabilitiesTimeout = 10 * time.Second

// NestedVirt asks hull whether it can boot a guest with nested virtualization,
// once per process: brig info asks for the text and the JSON form from one
// Config, and the answer cannot change while brig is running.
func (h *hull) NestedVirt() NestedSupport {
	h.nested.once.Do(func() { h.nested.s = h.askNestedVirt() })
	return h.nested.s
}

func (h *hull) askNestedVirt() NestedSupport {
	ctx, cancel := context.WithTimeout(context.Background(), capabilitiesTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.bin, "capabilities", "--json")
	cmd.WaitDelay = agentCallWaitDelay
	// Plumbing, never a user action: an uncounted question, or every brig info
	// would send a telemetry event through the real hull.
	cmd.Env = mergeEnv(telemetryEnv(false))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return NestedSupport{Detail: fmt.Sprintf("%s capabilities --json did not answer within %s",
				h.bin, capabilitiesTimeout)}
		}
		if unknownToHull(errb.String()) {
			return NestedSupport{Outdated: true, Detail: h.predatesNested()}
		}
		detail := fmt.Sprintf("%s capabilities --json: %v", h.bin, err)
		if said := strings.TrimSpace(errb.String()); said != "" {
			detail += ": " + firstLines(said, 1)
		}
		return NestedSupport{Detail: printable(detail)}
	}
	return parseCapabilities(h.bin, out.Bytes())
}

// unknownToHull reports whether hull turned a command or flag away as one it
// does not have, which is how a hull released before nested virtualization
// answers `capabilities --json` and `run --nested-virt`. These are the words of
// the CLI library hull is built on, not a message hull chose.
func unknownToHull(said string) bool {
	return strings.Contains(said, "flag provided but not defined") ||
		strings.Contains(said, "No help topic for")
}

// predatesNested is what brig says about a hull too old for nested
// virtualization. No minimum version is named, because none has been released
// yet; the version shown is the one this hull reports.
func (h *hull) predatesNested() string {
	return fmt.Sprintf("this hull (%s) predates nested virtualization; upgrade hull", h.versionLabel())
}

// capabilitiesDoc is the shape `hull capabilities --json` prints, schema 1.
type capabilitiesDoc struct {
	SchemaVersion int `json:"schemaVersion"`
	NestedVirt    *struct {
		Supported bool   `json:"supported"`
		Backend   string `json:"backend"`
		Detail    string `json:"detail"`
	} `json:"nestedVirt"`
}

// parseCapabilities reads hull's answer. Anything that is not a schema-1
// document carrying a nestedVirt object is not supported: an empty stdout, a
// stray line, a document from a hull that dropped the field. Another schema
// version is not read at all: a field this brig reads could mean something
// else there, and a wrong "supported" is the answer that costs a sandbox.
func parseCapabilities(bin string, blob []byte) NestedSupport {
	var doc capabilitiesDoc
	if err := json.Unmarshal(bytes.TrimSpace(blob), &doc); err != nil {
		return NestedSupport{Detail: fmt.Sprintf("%s capabilities --json printed nothing brig can read", bin)}
	}
	if doc.SchemaVersion != 1 {
		return NestedSupport{Detail: fmt.Sprintf("hull answered schemaVersion %d; this brig reads 1",
			doc.SchemaVersion)}
	}
	if doc.NestedVirt == nil {
		return NestedSupport{Detail: fmt.Sprintf("%s capabilities --json printed nothing brig can read", bin)}
	}
	n := doc.NestedVirt
	return NestedSupport{
		Supported: n.Supported,
		Backend:   printable(n.Backend),
		Detail:    printable(n.Detail),
	}
}

// printable keeps the runtime's words to one plain line. The detail goes to a
// terminal in brig info and brig doctor, and an escape sequence in it would act
// on that terminal.
func printable(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
}
