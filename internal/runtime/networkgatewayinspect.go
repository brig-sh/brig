package runtime

import (
	"fmt"
	"strings"
)

// Today's gateway environment cannot say how an existing guest booted. Only
// isolated gateways write a spec, so read beside the socket in the saved argv.
// This is gateway evidence, not VM identity: an old shared override at exactly
// that path could inherit a stale spec from an isolated gateway.
func inspectedGateway(name, socket string) (string, error) {
	control, ok := strings.CutSuffix(socket, ".qemu")
	if !ok || control == "" {
		return "", fmt.Errorf("inspect network of %s: unrecognised gateway socket", name)
	}
	if sandboxSocketName(control) {
		if recordedSpec(control) != "" {
			return "isolated", nil
		}
		// Stop removes the spec, and its write is best effort. Missing or
		// unreadable evidence cannot turn a legacy isolated guest into shared.
		return "", fmt.Errorf("inspect network of %s: gateway %q could be isolated or an older shared override; "+
			"no gateway spec is available beside the recorded socket", name, control)
	}
	return "shared", nil
}
