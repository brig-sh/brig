package wrap

import (
	"github.com/brig-sh/brig/internal/policy"
	"github.com/brig-sh/brig/internal/runtime"
)

// runtimeEgress is the one place a policy document becomes something a runtime
// takes.
//
// The two types have the same shape and are deliberately separate: a policy is
// a document brig reads, a RunSpec is a request brig makes, and the runtime
// adapters have no business knowing how a policy is spelled on disk. This
// function is the seam, so a rule class added to the document has exactly one
// place to be taught to travel.
func runtimeEgress(e policy.Egress) runtime.Egress {
	if e.Default == "" {
		return runtime.Egress{}
	}
	out := runtime.Egress{Default: e.Default}
	for _, r := range e.Allow {
		out.Allow = append(out.Allow, runtime.Rule{Host: r.Host, CIDR: r.CIDR})
	}
	for _, r := range e.Deny {
		out.Deny = append(out.Deny, runtime.Rule{Host: r.Host, CIDR: r.CIDR})
	}
	return out
}
