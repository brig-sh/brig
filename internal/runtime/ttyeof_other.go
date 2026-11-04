//go:build !linux

package runtime

import "os"

// consumePendingEOF has no effect here. brig hands a terminal to nerdctl on
// Linux only; the handover on macOS goes through hull, which this does not
// touch.
func consumePendingEOF(*os.File) {}
