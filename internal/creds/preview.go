package creds

import (
	"fmt"
	"sort"

	"github.com/brig-sh/brig/internal/profile"
)

// Planned is one credential a run hands the guest, as `brig plan` names it:
// where it lands, where its value comes from, and whether there is one. It
// carries no value, and the walk that builds it never holds one from the
// store.
type Planned struct {
	// Name is the guest variable for an env delivery, the path under the
	// guest home for a file, or the secret itself for a requirement no
	// binding delivers.
	Name     string
	Delivery string
	// Source is SourceEnvironment or SourceSecret for a resolved or withheld
	// credential, and empty when no source in the chain has a value.
	Source string
	// Secret is the store name the binding reads, when it reads the store.
	Secret   string
	Required bool
	State    string
	// Reason says why a credential is withheld, unresolved or unknown.
	Reason string
}

// How a credential reaches the guest. DeliveryNone is a required secret no
// binding delivers: the run still stops without it, so the plan names it.
const (
	DeliveryEnv  = "env"
	DeliveryFile = "file"
	DeliveryNone = "none"
)

// Where a credential's value comes from.
const (
	SourceEnvironment = "environment"
	SourceSecret      = "secret"
)

// What the run does with a credential. Withheld is a value the run has and
// drops at a guard. Unknown is a store that did not answer a listing, so the
// plan makes no claim either way.
const (
	StateResolved   = "resolved"
	StateUnresolved = "unresolved"
	StateWithheld   = "withheld"
	StateUnknown    = "unknown"
)

// Preview walks the same chains Bind walks and says what each delivers,
// without a secret value.
//
// stored answers for the store from a listing: whether a name is there, and
// whether the store answered at all. A listing reads no value. That lets a
// plan run on a host whose keychain prompts for a read, and keeps a plan from
// being one more place a value passes through.
//
// The price is that a listing cannot see what a read would see. Resolved from
// the store means the listing has the name, and nothing more. The run reads
// the value, and that read can still come back empty or fail:
//   - A value another tool emptied. A listing reads names and attributes,
//     and on macOS an empty secret still has a sealed ciphertext. Bind does
//     not deliver it: as the last element of a chain the run drops it with
//     the "empty, not absent" warning, and anywhere earlier Bind moves on to
//     the next element while this row names the empty one.
//   - A damaged item, such as a macOS secret whose sealed half is gone.
//   - A keychain or keyring prompt the user denies.
//   - A Linux collection that is locked.
//
// On the last three, ResolveSecrets fails the run for a required secret and
// drops an optional one with a warning, while this row says resolved.
//
// lookup is the shell, as Bind reads it. A shell value is read only to apply
// the same guards Bind applies, and it goes nowhere.
//
// The rows come out sorted by delivery and name, because a digest is taken
// over them and the profile's order is not a permission.
func Preview(
	p profile.Profile,
	bindings []profile.EnvBinding,
	stored func(name string) (present, known bool),
	lookup func(string) (string, bool),
	opt Options,
) []Planned {
	var out []Planned
	delivered := map[string]bool{}
	for _, b := range bindings {
		refs := b.RefList()
		if len(refs) == 0 {
			// A literal is configuration the profile sets, not a credential.
			continue
		}
		c := Planned{Name: b.Name, Delivery: DeliveryEnv, State: StateUnresolved}
		unlisted := false
		for i, raw := range refs {
			r, err := profile.ParseRef(raw)
			if err != nil {
				// Bind forwards nothing for this binding, whatever the chain
				// passed on the way here.
				c.Secret, c.Required, unlisted = "", false, false
				c.Reason = err.Error()
				break
			}
			last := i == len(refs)-1
			if r.Namespace == profile.NamespaceEnv {
				v, ok := lookup(r.Name)
				if !ok || v == "" {
					continue
				}
				c.Source, c.Secret, c.Required = SourceEnvironment, "", false
				c.State = StateResolved
				if warning, ok := admit(p, b.Name, v, true, opt); !ok {
					c.State, c.Reason = StateWithheld, warning
				}
				break
			}
			c.Secret, c.Required = r.Name, required(p, r.Name)
			present, known := stored(r.Name)
			if !known {
				// Bind falls through a secret the run did not read, so a
				// later element still answers. A chain nothing later
				// answers is unknown.
				unlisted = true
				continue
			}
			if !present {
				if last {
					c.Reason = fmt.Sprintf("the secret %s is not in the store", r.Name)
				}
				continue
			}
			c.Source, c.State = SourceSecret, StateResolved
			if warning, ok := admit(p, b.Name, "", false, opt); !ok {
				c.State, c.Reason = StateWithheld, warning
			}
			break
		}
		if c.State == StateUnresolved && unlisted {
			c.State, c.Reason = StateUnknown, "the secret store did not answer a listing"
		}
		if c.State == StateUnresolved && c.Reason == "" {
			c.Reason = "no source in the chain has a value"
		}
		// Only the secret the row ends on counts as delivered. A required
		// secret the chain passed over still stops the run (see Needed), so
		// it gets its own row below.
		if c.Secret != "" {
			delivered[c.Secret] = true
		}
		out = append(out, c)
	}
	for _, b := range p.Files {
		r, err := profile.ParseRef(b.Ref)
		if err != nil {
			out = append(out, Planned{Name: b.Path, Delivery: DeliveryFile,
				State: StateUnresolved, Reason: err.Error()})
			continue
		}
		delivered[r.Name] = true
		out = append(out, secretRow(p, b.Path, DeliveryFile, r.Name, stored))
	}
	// A required secret stops the run whether or not a binding delivers it
	// (see Needed). A plan that left it out promises a run that then fails.
	for _, d := range p.Secrets {
		if d.IsRequired() && !delivered[d.Name] {
			out = append(out, secretRow(p, d.Name, DeliveryNone, d.Name, stored))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Delivery != out[j].Delivery {
			return out[i].Delivery < out[j].Delivery
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// secretRow is a credential with one source, the store, which is every file
// binding and every requirement.
func secretRow(p profile.Profile, name, delivery, secretName string,
	stored func(string) (bool, bool)) Planned {
	c := Planned{Name: name, Delivery: delivery, Secret: secretName, Required: required(p, secretName)}
	switch present, known := stored(secretName); {
	case !known:
		c.State, c.Reason = StateUnknown, "the secret store did not answer a listing"
	case present:
		c.Source, c.State = SourceSecret, StateResolved
	default:
		c.State, c.Reason = StateUnresolved, fmt.Sprintf("the secret %s is not in the store", secretName)
	}
	return c
}

// required reads the declaration. A secret nothing declares is not required,
// the same reading Needed gives it.
func required(p profile.Profile, name string) bool {
	d, ok := p.Secret(name)
	return ok && d.IsRequired()
}
