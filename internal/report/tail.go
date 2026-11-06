package report

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// tailFile returns the last n lines of path. With grep set, it returns the
// last n lines that contain it, which needs a full read; without, it reads
// backwards from the end, so a console log of hundreds of megabytes costs
// what its last lines cost.
func tailFile(path string, n int, grep string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if grep != "" {
		return tailMatching(f, n, grep)
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	return tailReaderAt(f, info.Size(), n)
}

const tailChunk = 64 << 10

// tailReaderAt reads chunks backwards from size until it holds n full lines.
func tailReaderAt(r io.ReaderAt, size int64, n int) (string, error) {
	if n <= 0 || size == 0 {
		return "", nil
	}
	var buf []byte
	off := size
	for off > 0 {
		chunk := int64(tailChunk)
		if chunk > off {
			chunk = off
		}
		off -= chunk
		part := make([]byte, chunk)
		if _, err := r.ReadAt(part, off); err != nil && err != io.EOF {
			return "", err
		}
		buf = append(part, buf...)
		// A trailing newline ends the last line, so n lines need n+1
		// newlines to know where the first of them starts.
		if strings.Count(string(buf), "\n") > n {
			break
		}
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
			return s, nil
		}
		start = i
	}
	return s[start+1:], nil
}

// maxLine is the longest line tailMatching keeps whole. A longer one is cut,
// so one runaway line in a log does not cost the report every other line.
const maxLine = 64 << 10

// tailMatching keeps the last n lines of r that contain grep.
func tailMatching(r io.Reader, n int, grep string) (string, error) {
	if n <= 0 {
		return "", nil
	}
	ring := make([]string, 0, n)
	br := bufio.NewReaderSize(r, maxLine)
	for {
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
	return line + " [line cut]", err
}
