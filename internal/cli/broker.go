package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/brig-sh/brig/internal/injector"
)

// brokerUsage is what `brig broker --help` prints.
const brokerUsage = `brig broker -- revoke a sandbox's broker access, or reinstate it

usage:
  brig broker revoke <sandbox> [--reason TEXT]   drop the sandbox's route
                                                 tokens and refuse every
                                                 registration of it
  brig broker reinstate <sandbox>                lift a revocation
  brig broker revoked                            list the revoked sandboxes

<sandbox> is the SANDBOX column of brig ls, such as brig-claude-code.

A revocation holds until it is reinstated: brig stop, brig rm and a restart
of the broker keep it. While it holds, brig run and brig sh refuse the
sandbox when it has a route, so an agent in it gets no new token. A sandbox
that has no route is not affected.

docs/broker.md describes the revocation and the control call another tool
can send instead.
`

// brokerCmd revokes or reinstates a sandbox's broker access.
func brokerCmd(out io.Writer, args []string) error {
	if len(args) == 0 {
		return usagef("broker takes a subcommand: revoke, reinstate or revoked")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "--help", "-h", "help":
		_, err := io.WriteString(out, brokerUsage)
		return err
	case "revoke":
		id, reason, err := brokerOperand(sub, rest, true)
		if err != nil {
			return err
		}
		dir, err := injector.Dir()
		if err != nil {
			return err
		}
		got, err := injector.Revoke(dir, id, injector.RevokeRequest{Reason: reason})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s: broker access revoked at %s\n", id, got.Revocation.At.Format(time.RFC3339))
		return err
	case "reinstate":
		id, _, err := brokerOperand(sub, rest, false)
		if err != nil {
			return err
		}
		dir, err := injector.Dir()
		if err != nil {
			return err
		}
		got, err := injector.Reinstate(dir, id)
		if err != nil {
			return err
		}
		if !got.Reinstated {
			_, err = fmt.Fprintf(out, "%s: broker access was not revoked\n", id)
			return err
		}
		_, err = fmt.Fprintf(out, "%s: broker access reinstated; its next run registers its routes\n", id)
		return err
	case "revoked":
		if len(rest) > 0 {
			return usagef("brig broker revoked takes no argument")
		}
		dir, err := injector.Dir()
		if err != nil {
			return err
		}
		list, err := injector.Revocations(dir)
		if err != nil {
			return err
		}
		for _, r := range list {
			line := []string{r.ID, r.At.Format(time.RFC3339)}
			if r.By != "" {
				line = append(line, "by "+r.By)
			}
			if r.Reason != "" {
				line = append(line, r.Reason)
			}
			if _, err := fmt.Fprintln(out, strings.Join(line, "  ")); err != nil {
				return err
			}
		}
		return nil
	}
	return usagef("unknown broker subcommand %q (revoke, reinstate or revoked)", sub)
}

// brokerOperand reads the sandbox of a revoke or reinstate, and the reason
// a revoke may carry.
func brokerOperand(sub string, args []string, reasonOK bool) (string, string, error) {
	var id, reason string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case reasonOK && a == "--reason":
			if i+1 == len(args) {
				return "", "", usagef("--reason takes a value")
			}
			i++
			reason = args[i]
		case reasonOK && strings.HasPrefix(a, "--reason="):
			reason = strings.TrimPrefix(a, "--reason=")
		case strings.HasPrefix(a, "-"):
			return "", "", usagef("brig broker %s does not take %s", sub, a)
		case id != "":
			return "", "", usagef("brig broker %s takes one sandbox", sub)
		default:
			id = a
		}
	}
	if id == "" {
		return "", "", usagef("brig broker %s takes a sandbox, as brig ls names it", sub)
	}
	if strings.ContainsAny(id, "@/ ") {
		return "", "", errors.New("name the sandbox as the SANDBOX column of brig ls does, such as " +
			"brig-claude-code, not the ref")
	}
	return id, reason, nil
}
