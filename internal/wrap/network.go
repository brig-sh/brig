package wrap

import "fmt"

// Network is the posture a sandbox runs with.
type Network string

const (
	// NetShared is one network per host: every sandbox on it can reach every
	// other. What brig has always done, and still the default.
	NetShared Network = "shared"
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
	case string(NetOffline):
		return NetOffline, nil
	default:
		return NetShared, fmt.Errorf("%s %q is not a posture: use shared or offline", source, s)
	}
}

// RuntimeNet is the word the runtime adapters take for this posture. The two
// vocabularies are deliberately separate: "offline" is what a person asks for,
// "none" is what a runtime is told.
func (n Network) RuntimeNet() string {
	if n == NetOffline {
		return "none"
	}
	return "shared"
}

// Line is the posture as a reader is told it, with the consequence spelled
// out: "offline" on its own is a word, and what a reader needs is what it
// costs them.
//
// Beside RuntimeNet deliberately. A posture has two vocabularies, the
// runtime's and the reader's, and keeping both on the type means a posture
// added later cannot pick up one translation and quietly miss the other.
func (n Network) Line() string {
	if n == NetOffline {
		return "offline (no egress)"
	}
	return "shared (sandboxes on this host can reach each other)"
}
