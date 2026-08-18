package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A secret is a credential, and the store this feeds takes about 3 KB. Reading
// stdin to the end first meant `brig secret create` pointed at a stream sat
// there allocating: 12.5 GB resident three seconds in, on the way to refusing
// the value for being too long. The read stops a little above what can be
// stored, and says so.
func TestCreateRefusesAValueThatDoesNotEnd(t *testing.T) {
	f := newFake(t)
	pipeStdin(t, strings.Repeat("A", 1<<20))
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"})
	if err == nil {
		t.Fatal("a megabyte on stdin was accepted as a secret")
	}
	if !strings.Contains(err.Error(), "stdin") || !strings.Contains(err.Error(), "4096") {
		t.Errorf("the refusal does not say what it refused or where the ceiling is: %v", err)
	}
	if _, ok := f.items["gh"]; ok {
		t.Error("it was stored anyway")
	}
}

// -f reads the same way and needs the same ceiling: a path can name a stream
// as easily as stdin can be one.
func TestCreateFromAFileIsCappedToo(t *testing.T) {
	newFake(t)
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, bytes.Repeat([]byte("A"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	err := secretCmd(&bytes.Buffer{}, []string{"create", "gh", "-f", path})
	if err == nil {
		t.Fatal("a megabyte in a file was accepted as a secret")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// A value at the ceiling still goes through: the cap is a refusal of streams,
// not a new limit on secrets.
func TestCreateStillTakesAnOrdinarySizedSecret(t *testing.T) {
	f := newFake(t)
	value := strings.Repeat("k", 2048)
	pipeStdin(t, value)
	if err := secretCmd(&bytes.Buffer{}, []string{"create", "gh"}); err != nil {
		t.Fatalf("an ordinary secret was refused: %v", err)
	}
	if string(f.items["gh"]) != value {
		t.Error("the value was truncated")
	}
}

// endlessReader is the /dev/zero the delete prompt was pointed at, counting
// what it was asked for.
type endlessReader struct{ read int }

func (e *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'y'
	}
	e.read += len(p)
	return len(p), nil
}

// The delete prompt read until it found a newline, so an answer that never
// ended one was an answer that never ended: 6.75 GB of resident memory from
// /dev/zero. Nothing past the first line can change a yes-or-no answer, so
// nothing past it is read.
func TestTheDeleteAnswerReadIsBounded(t *testing.T) {
	stream := &endlessReader{}
	answer, err := readAnswer(stream)
	if err == nil {
		t.Errorf("a stream with no line in it was accepted as an answer: %q", answer)
	}
	if stream.read > maxAnswerBytes {
		t.Errorf("read %d bytes looking for a one-line answer, cap is %d",
			stream.read, maxAnswerBytes)
	}
	// And the refusal is not-yes, which is what the caller acts on.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y") && err == nil {
		t.Error("an unbounded stream answered yes")
	}
}

// A real answer is still read, ceiling or no ceiling.
func TestTheDeleteAnswerStillReadsAYes(t *testing.T) {
	answer, err := readAnswer(strings.NewReader("yes\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(answer) != "yes" {
		t.Errorf("answer = %q", answer)
	}
}

var _ io.Reader = (*endlessReader)(nil)
