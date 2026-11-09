package runtime

import (
	"bytes"
	"fmt"
)

// maxOutput is the most Output keeps of a guest command's standard output.
// The guest decides how much it prints, and a buffer without a cap would let
// it grow brig's memory until the call's deadline. Every caller reads a
// small file or a short listing, so the cap is far above any honest answer.
const maxOutput = 4 << 20

// cappedOutput keeps the first max bytes written to it and drops the rest.
// It still takes every write, so the guest is not left blocked on a full
// pipe until the deadline.
type cappedOutput struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.over = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

// result is the output, or an error when the guest printed more than the
// cap: a cut answer could read as a whole one.
func (c *cappedOutput) result() (string, error) {
	if c.over {
		return "", fmt.Errorf("the guest printed more than %d MiB; brig keeps at most that", c.max>>20)
	}
	return c.buf.String(), nil
}
