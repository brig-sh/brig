package wrap

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/brig-sh/brig/internal/verify"
)

// verifyImage checks the guest image before booting it, and decides what to
// do about the answer.
//
// The rule is deliberately asymmetric. An image nobody claimed to publish is
// reported and booted: bring-your-own images are a supported way to use brig,
// and blocking one would make the feature useless. An image that sits under
// our registry and fails verification is the one case that stops, because
// that combination has no innocent reading -- it means something is
// pretending to be us.
func (c *Config) verifyImage() error {
	if c.Verify == verify.Off {
		return nil
	}
	res := c.VerifyPolicy.Image(c.Image)

	switch res.Outcome {
	case verify.Verified:
		c.warnf("%s", res.Message())
		return nil

	case verify.NotOurs, verify.NoTooling:
		if c.Verify == verify.Require {
			return fmt.Errorf("%s (BRIG_VERIFY=require)", res.Message())
		}
		c.warnf("%s", res.Message())
		return nil

	default:
		c.warnf("%s", res.Message())
		if c.Verify == verify.Require {
			return errors.New("refusing to boot an image that failed verification")
		}
		if !c.confirm("Boot it anyway?") {
			return errors.New("aborted: the image failed verification. " +
				"Pull it again, or set BRIG_VERIFY=off if you know why it fails")
		}
		return nil
	}
}

// confirm asks a yes/no question, defaulting to no.
//
// Without a terminal there is nobody to ask, and assuming yes would turn the
// one check that stops into one that does not. So it answers no and says
// which setting overrides it -- a scripted run that genuinely wants to
// proceed can say so in advance.
//
// A caller that has no terminal of its own says so with NoTerminal rather than
// leaving it to be guessed from this process's stdin. The two are not the same
// question for a daemon: brigd's stdin may well be a terminal, it is simply
// not the one the request came from, and asking there puts the question in
// front of nobody while the client waits.
func (c *Config) confirm(question string) bool {
	if c.NoTerminal || !IsTerminal(os.Stdin) {
		c.warnf("not a terminal, so there is nobody to ask: refusing. " +
			"Set BRIG_VERIFY=off to boot it regardless.")
		return false
	}
	fmt.Fprintf(c.Err, "brig: %s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
