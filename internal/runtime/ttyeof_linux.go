package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

// consumePendingEOF reads the end-of-file marks queued on f, so that the raw
// switch of the handover that follows cannot turn them into data.
//
// script(1) sends the terminal's EOF character when its own stdin ends, and
// with </dev/null that happens before brig has started. In canonical mode the
// line discipline queues the character as an empty line, and a read returns 0
// for it. nerdctl v2.3.5 starts its stdin copy before it calls term.MakeRaw,
// so when the raw switch wins the race the copy reads the mark as a literal
// 0x00. That byte reaches the guest pty, which echoes it as "^@"
// (brig-sh/brig#460). Reading here, while the terminal is still canonical,
// consumes the mark as the EOF it is. A mark that arrives after this call and
// before nerdctl's raw switch is still turned into 0x00; only nerdctl can close
// that window.
//
// Only lone marks are read. A typed line stays queued, so it reaches the guest.
// The loop is bounded at 16 so that a driver that keeps sending marks cannot
// hold up the handover. Every failure returns silently: this is best effort
// before an exec that must neither fail nor print because of it, and a mark
// left behind is still read as EOF by the next canonical read.
func consumePendingEOF(f *os.File) {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil || t.Lflag&unix.ICANON == 0 {
		return
	}
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for i := 0; i < 16; i++ {
		if n, err := unix.Poll(pfd, 0); err != nil || n != 1 || pfd[0].Revents&unix.POLLIN == 0 {
			return
		}
		// In canonical mode TIOCINQ counts the bytes of complete lines and not
		// the EOF mark, so readable with nothing counted is a lone mark. A count
		// above zero is a typed line, which is left for the guest.
		if n, err := unix.IoctlGetInt(fd, unix.TIOCINQ); err != nil || n != 0 {
			return
		}
		var b [1]byte
		if n, err := unix.Read(fd, b[:]); err != nil || n != 0 {
			return
		}
	}
}
