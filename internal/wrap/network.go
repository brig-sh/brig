package wrap

import "fmt"

// Network is the posture a sandbox runs with.
type Network string

const (
	// NetShared is one network for every sandbox on the host. What brig has
	// always done, and still the default. Whether the sandboxes on it can
	// reach each other is the backend's answer, not brig's: on Linux they can,
	// on both macOS backends they cannot. See docs/security.md.
	NetShared Network = "shared"
	// NetIsolated is a network of this sandbox's own, so no other sandbox is
	// on it whatever the backend does with a shared one.
	NetIsolated Network = "isolated"
	// NetOffline is a sandbox with no route out. The agent runs, the workspace
	// is mounted, nothing leaves.
	NetOffline Network = "offline"
)

// ParseNetworkStrict reads a posture and refuses anything it does not
// recognise. source is how the value reached brig -- a flag, a setting, a
// profile field -- and it is named in the refusal, because being told about
// BRIG_NETWORK when you typed --network sends you looking for a variable you
// never set.
//
// Strict for the same reason the security switches are: this decides whether a
// sandbox can reach the network at all, so a typo must stop the run rather
// than quietly pick a posture nobody asked for. The empty string is the unset
// case and keeps the default.
func ParseNetworkStrict(s, source string) (Network, error) {
	switch s {
	case "":
		return NetShared, nil
	case string(NetShared):
		return NetShared, nil
	case string(NetIsolated):
		return NetIsolated, nil
	case string(NetOffline):
		return NetOffline, nil
	default:
		return NetShared, fmt.Errorf("%s %q is not a posture: use shared, isolated or offline",
			source, s)
	}
}

// AllNetworks is every posture that exists. A list rather than a comment, so a
// test can walk it and catch a posture that gained a case in one switch and
// not the other.
func AllNetworks() []Network { return []Network{NetShared, NetIsolated, NetOffline} }

// RuntimeNet is the word the runtime adapters take for this posture. The two
// vocabularies are deliberately separate: "offline" is what a person asks for,
// "none" is what a runtime is told.
func (n Network) RuntimeNet() string {
	switch n {
	case NetOffline:
		return "none"
	case NetIsolated:
		return "isolated"
	default:
		return "shared"
	}
}

// Line is the posture as a reader is told it, with the consequence spelled
// out: "offline" on its own is a word, and what a reader needs is what it
// costs them.
//
// Beside RuntimeNet deliberately. A posture has two vocabularies, the
// runtime's and the reader's, and keeping both on the type means a posture
// added later cannot pick up one translation and quietly miss the other.
//
// The shared line describes the topology and stops there. Whether one sandbox
// can actually reach another on that network is a property of the backend, not
// of this setting. On both macOS backends they cannot: a packet capture shows
// an ARP broadcast crossing between guests on hvi, and the unicast reply not
// crossing back, so neither guest ever learns the other's address. On Linux
// they can. A row claiming either would be false somewhere, so it claims what
// is true everywhere: these sandboxes are on one network rather than each on
// its own.
func (n Network) Line() string {
	switch n {
	case NetOffline:
		return "offline (no egress)"
	case NetIsolated:
		return "isolated (a network of this sandbox's own)"
	default:
		return "shared (one network for every sandbox on this host)"
	}
}
