package report

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
)

// tailFile returns the last n lines of path. With grep set, it returns the
// last n lines that contain it, which needs a full read; without, it reads
// backwards from the end, so a console log of hundreds of megabytes costs
// what its last lines cost. Either stops when ctx ends.
func tailFile(ctx context.Context, path string, n int, grep string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if grep != "" {
		return tailMatching(ctx, f, n, grep)
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	return tailReaderAt(ctx, f, info.Size(), n)
}

const tailChunk = 64 << 10

// maxTail is the most tailReaderAt reads back from the end. A log whose
// lines are carriage returns, as a progress bar writes them, has no newline
// to stop at and would otherwise be read whole into the report.
const maxTail = 1 << 20

// tailReaderAt reads chunks backwards from size until it holds n full lines,
// maxTail bytes, or ctx ends.
func tailReaderAt(ctx context.Context, r io.ReaderAt, size int64, n int) (string, error) {
	if n <= 0 || size == 0 {
		return "", nil
	}
	var buf []byte
	off := size
	newlines := 0
	cut := false
	// A trailing newline ends the last line, so n lines need n+1 newlines
	// to know where the first of them starts.
	for off > 0 && newlines <= n {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if size-off >= maxTail {
			cut = true
			break
		}
		chunk := min(int64(tailChunk), off, maxTail-(size-off))
		off -= chunk
		part := make([]byte, chunk)
		if _, err := r.ReadAt(part, off); err != nil && err != io.EOF {
			return "", err
		}
		// Count only the new chunk: recounting the whole buffer each time
		// costs the square of the bytes read.
		newlines += bytes.Count(part, []byte{'\n'})
		buf = append(part, buf...)
	}
	s := string(buf)
	end := len(s)
	if strings.HasSuffix(s, "\n") {
		end--
	}
	start := end
	for range n {
		i := strings.LastIndexByte(s[:start], '\n')
		if i < 0 {
			start = -1
			break
		}
		start = i
	}
	s = s[start+1:]
	if cut && start < 0 {
		s = "[earlier text cut] " + s
	}
	return cutLines(s), nil
}

// maxLine is the longest line the report keeps whole. A longer one is cut,
// so one runaway line in a log does not cost the report every other line.
const maxLine = 64 << 10

// cutLines cuts each line of s over maxLine.
func cutLines(s string) string {
	if len(s) <= maxLine {
		return s
	}
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if len(l) > maxLine {
			lines[i] = cut(l[:maxLine]) + " [line cut]" + l[len(strings.TrimRight(l, "\n")):]
		}
	}
	return strings.Join(lines, "")
}

// cut drops a rune the byte limit split, so the report stays valid UTF-8.
func cut(s string) string { return strings.ToValidUTF8(s, "") }

// tailMatching keeps the last n lines of r that contain grep.
func tailMatching(ctx context.Context, r io.Reader, n int, grep string) (string, error) {
	if n <= 0 {
		return "", nil
	}
	ring := make([]string, 0, n)
	br := bufio.NewReaderSize(r, maxLine)
	for i := 0; ; i++ {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		line, err := readLine(br)
		if line != "" && strings.Contains(line, grep) {
			if len(ring) == n {
				ring = append(ring[:0], ring[1:]...)
			}
			ring = append(ring, line)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if len(ring) == 0 {
		return "", nil
	}
	return strings.Join(ring, "\n") + "\n", nil
}

// readLine returns the next line without its newline. A line longer than the
// reader's buffer comes back cut at the buffer's size, marked, and the rest of
// it is skipped.
func readLine(br *bufio.Reader) (string, error) {
	part, err := br.ReadSlice('\n')
	line := strings.TrimSuffix(string(part), "\n")
	if err != bufio.ErrBufferFull {
		return line, err
	}
	for err == bufio.ErrBufferFull {
		_, err = br.ReadSlice('\n')
	}
	return cut(line) + " [line cut]", err
}
