package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/runtime"
)

// --publish is brig's, repeatable, and it reaches Load as written.
func TestPublishFlagCollects(t *testing.T) {
	o, _, tail, err := parse("run", []string{"--publish", "3000", "claude", "--publish", "8080:80"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(o.load.Publish, " "); got != "3000 8080:80" {
		t.Fatalf("publish = %q, want both, in order", got)
	}
	if len(tail) != 0 {
		t.Fatalf("tail = %v, want nothing left for the agent", tail)
	}
}

// -p is NOT brig's. It is claude's print flag and docker's publish flag, and
// brig owns only one of those readings -- so `brig run claude -p "fix the
// tests"` has to stay a prompt. TestBrigFlagsOverlapWithShippedAgentsOnlyWhereKnown
// guards the table; this guards the line.
func TestShortPStaysTheAgents(t *testing.T) {
	o, _, tail, err := parse("run", []string{"claude", "-p", "fix the tests"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(o.load.Publish) != 0 {
		t.Fatalf("brig read -p as a published port: %v", o.load.Publish)
	}
	if strings.Join(tail, " ") != "-p fix the tests" {
		t.Fatalf("tail = %v, want the agent's own flag untouched", tail)
	}
}

// The network publish verbs take their ports as bare words, which is the position
// an agent's argv would otherwise occupy -- and they have no agent.
func TestPublishVerbsTakeBareWordsAsPorts(t *testing.T) {
	for _, verb := range []string{"network publish", "network unpublish"} {
		_, profileName, tail, err := parse(verb, []string{"claude@web", "8080:80", "3000"})
		if err != nil {
			t.Fatalf("parse %s: %v", verb, err)
		}
		if profileName != "claude" {
			t.Errorf("%s: profile = %q", verb, profileName)
		}
		if strings.Join(tail, " ") != "8080:80 3000" {
			t.Errorf("%s: ports = %v", verb, tail)
		}
	}
}

// --all is legal on the publish line and nowhere else; a run-line flag is not
// legal there at all, because those verbs shape no run.
func TestPublishLineHasAVocabularyOfItsOwn(t *testing.T) {
	o, _, tail, err := parse("network unpublish", []string{"claude@web", "--all"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !o.all {
		t.Error("--all was not read")
	}
	if len(tail) != 0 {
		t.Errorf("tail = %v", tail)
	}
	// A run-line flag here is a mistake to name rather than a port to fail on
	// with a message about port numbers.
	_, _, _, err = parse("network publish", []string{"claude@web", "--mem", "4096"})
	if err == nil {
		t.Fatal("--mem on the publish line was accepted")
	}
	if !strings.Contains(err.Error(), "--mem") || !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("the refusal does not name the flag: %v", err)
	}
}

// Every verb that takes ports keeps its tail, and rejectTail must then not
// refuse it. The two predicates read the same set for exactly this reason.
func TestPortVerbsKeepTheirOperands(t *testing.T) {
	for _, verb := range []string{"network publish", "network unpublish"} {
		if !takesPorts(verb) {
			t.Fatalf("%s does not take ports", verb)
		}
		if err := rejectTail(verb, []string{"8080"}); err != nil {
			t.Errorf("%s refused its own port: %v", verb, err)
		}
		if forwardsTail(verb) {
			t.Errorf("%s forwards a tail to an agent it does not have", verb)
		}
	}
	// And a verb that takes neither still refuses a stray word.
	if err := rejectTail("stop", []string{"8080"}); err == nil {
		t.Error("stop accepted a trailing word")
	}
}

// #228: publish and unpublish take their ports as bare words, and split handed
// the whole tail to the port parser at the first one. `brig network publish
// claude@web 8080 --json` then read --json as a second port and refused the
// line, so only the global spelling worked.
func TestJSONOnAPublishLineIsNotAPort(t *testing.T) {
	for _, verb := range []string{"network publish", "network unpublish"} {
		o, _, tail, err := parse(verb, []string{"claude@web", "8080", "--json"})
		if err != nil {
			t.Fatalf("parse(%s): %v", verb, err)
		}
		if !o.json {
			t.Errorf("%s: --json after a port did not reach brig", verb)
		}
		if got := strings.Join(tail, " "); got != "8080" {
			t.Errorf("%s: ports = %q, want the one bare word", verb, got)
		}
	}
}

// #228: nerdctl implements no Publisher, so brig cannot ask it whether a port
// is forwarded. That is not the same answer as a port which is not, and
// reporting false for it called every open port on a Linux host shut.
func TestPortStateIsUnknownWhenNothingCanAnswer(t *testing.T) {
	open, shut := true, false
	var buf bytes.Buffer
	printPorts(&buf, []portRow{
		{Host: "127.0.0.1:3000", Guest: 3000, Protocol: "tcp"},
		{Host: "127.0.0.1:3001", Guest: 3001, Protocol: "tcp", Live: &shut},
		{Host: "127.0.0.1:3002", Guest: 3002, Protocol: "tcp", Live: &open},
	})
	for _, want := range []string{"unknown", "pending", "open"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("printPorts did not report %q:\n%s", want, buf.String())
		}
	}
}

// #312: the port verbs live under `brig network`. The flat spellings never
// shipped, so nothing keeps them working.
func TestTheFlatPortVerbsAreGone(t *testing.T) {
	jsonRunHost(t, &jsonRuntime{})
	for _, verb := range []string{"publish", "unpublish"} {
		err := run([]string{verb, "faker", "3000"})
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("brig %s: err = %v, want an unknown command", verb, err)
		}
	}
}

func TestNetworkGroupAnswersHelpAndRefusesAStrangeWord(t *testing.T) {
	jsonRunHost(t, &jsonRuntime{})
	for _, line := range [][]string{{"network"}, {"network", "--help"}} {
		out, err := captureStdout(t, func() error { return run(line) })
		if err != nil {
			t.Fatalf("brig %s: %v", strings.Join(line, " "), err)
		}
		if !strings.Contains(out, "brig network unpublish") {
			t.Errorf("brig %s printed no usage:\n%s", strings.Join(line, " "), out)
		}
	}
	err := run([]string{"network", "open", "faker", "3000"})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), `"open"`) {
		t.Errorf("an unknown subcommand: err = %v, want a usage error naming it", err)
	}
}

// ls is the listing. publish with no port used to be it, and now points there.
func TestNetworkLsListsAndPublishNeedsAPort(t *testing.T) {
	jsonRunHost(t, &jsonRuntime{})
	out, err := captureStdout(t, func() error { return run([]string{"network", "ls", "faker"}) })
	if err != nil {
		t.Fatalf("brig network ls: %v", err)
	}
	if !strings.Contains(out, "faker publishes no ports") {
		t.Errorf("brig network ls printed %q", out)
	}
	out, err = captureStdout(t, func() error {
		return run([]string{"--json", "network", "ls", "faker"})
	})
	if err != nil {
		t.Fatalf("brig --json network ls: %v", err)
	}
	if !strings.Contains(out, `"Ports"`) {
		t.Errorf("brig --json network ls printed %q", out)
	}
	err = run([]string{"network", "publish", "faker"})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "brig network ls faker") {
		t.Errorf("publish with no port: err = %v, want a usage error naming network ls", err)
	}
}

func TestNetworkLsTakesNoPorts(t *testing.T) {
	jsonRunHost(t, &jsonRuntime{})
	err := run([]string{"network", "ls", "faker", "3000"})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "`brig network ls` takes a ref") {
		t.Errorf("err = %v, want a usage error for the stray port", err)
	}
}

// --all is read on every network line, but only unpublish withdraws with it.
func TestAllBelongsToUnpublishAlone(t *testing.T) {
	jsonRunHost(t, &jsonRuntime{})
	for _, sub := range []string{"ls", "publish"} {
		err := run([]string{"network", sub, "faker", "--all"})
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "brig network unpublish") {
			t.Errorf("brig network %s --all: err = %v, want a usage error", sub, err)
		}
	}
}

// bootFailRuntime fails every boot and lists no sandbox.
type bootFailRuntime struct{ jsonRuntime }

func (r *bootFailRuntime) Run(runtime.RunSpec) error         { return errors.New("boot failed") }
func (r *bootFailRuntime) List() ([]runtime.Instance, error) { return nil, nil }

// A run that did not boot must not leave its --publish behind. The next plain
// run would otherwise open the port without being asked.
func TestAFailedBootRecordsNoPort(t *testing.T) {
	jsonRunHost(t, &bootFailRuntime{})
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	if err := run([]string{"run", "--publish", "0.0.0.0:8080:80", "faker"}); err == nil {
		t.Fatal("the boot failed and the run did not")
	}
	got, err := runtime.Publications("brig-faker")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a run that never booted recorded %v", got)
	}
}

// `brig network publish` records a port for a sandbox that has never booted.
// rm finds no sandbox there, and still drops the record, or the next run
// would open the port.
func TestRemovingANeverBootedSandboxDropsItsPorts(t *testing.T) {
	jsonRunHost(t, &bootFailRuntime{})
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	if _, err := captureStdout(t, func() error {
		return run([]string{"network", "publish", "faker", "3000"})
	}); err != nil {
		t.Fatalf("network publish: %v", err)
	}
	if got, _ := runtime.Publications("brig-faker"); len(got) != 1 {
		t.Fatalf("the port was not recorded: %v", got)
	}
	if err := run([]string{"rm", "faker"}); exitCode(err) != 3 {
		t.Fatalf("rm of a sandbox that is not there: err = %v, want a not-found", err)
	}
	if got, _ := runtime.Publications("brig-faker"); len(got) != 0 {
		t.Fatalf("rm left the ports behind: %v", got)
	}
}

// stoppedPublisher can publish, and has no gateway to ask because nothing is
// running.
type stoppedPublisher struct{ bootFailRuntime }

func (r *stoppedPublisher) Publish(string, runtime.Publication) error   { return nil }
func (r *stoppedPublisher) Unpublish(string, runtime.Publication) error { return nil }
func (r *stoppedPublisher) Published(string) ([]runtime.Publication, error) {
	return nil, errors.New("no gateway serves a stopped sandbox")
}

// A stopped sandbox forwards nothing, and its ports read pending whichever
// network it is on. Asking the gateway anyway read unknown for an isolated
// one, whose gateway stops with it.
func TestAStoppedSandboxsPortsArePending(t *testing.T) {
	jsonRunHost(t, &stoppedPublisher{})
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	p, _ := runtime.ParsePublications([]string{"3000"})
	if _, err := runtime.RecordPublications("brig-faker", p); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return run([]string{"network", "ls", "faker"}) })
	if err != nil {
		t.Fatalf("network ls: %v", err)
	}
	if !strings.Contains(out, "pending") {
		t.Errorf("a stopped sandbox's port does not read pending:\n%s", out)
	}
}

// A -- on a port line ends brig's flags, and keeps the ports read before it.
func TestAMarkerKeepsThePortsBeforeIt(t *testing.T) {
	_, _, tail, err := parse("network publish", []string{"claude", "3000", "--", "4000"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(tail, " "); got != "3000 4000" {
		t.Fatalf("ports = %q, want both", got)
	}
}

// existsRuntime is a stoppedPublisher that can say whether a sandbox exists.
type existsRuntime struct {
	stoppedPublisher
	exists bool
}

func (r *existsRuntime) Exists(string) (bool, error) { return r.exists, nil }

// publishNotice runs `brig network publish` and returns what it said on
// stderr.
func publishNotice(t *testing.T, ref, port string) string {
	t.Helper()
	var err error
	said := captureStderr(t, func() {
		_, err = captureStdout(t, func() error {
			return run([]string{"network", "publish", ref, port})
		})
	})
	if err != nil {
		t.Fatalf("network publish %s %s: %v", ref, port, err)
	}
	return said
}

// A stress test published onto ubuntu@nosuch, a ref with no sandbox, and was
// told that brig-ubuntu-nosuch "is not running". Publishing ahead of a first
// run is allowed, so the command succeeds. The notice says that the ref has
// no sandbox, which run takes the port up, and how to drop it.
func TestPublishAheadOfTheFirstRunSaysSo(t *testing.T) {
	jsonRunHost(t, &existsRuntime{})
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	said := publishNotice(t, "faker@nosuch", "18083")
	for _, want := range []string{
		"faker@nosuch has no sandbox yet",
		"the next `brig run faker@nosuch` publishes them",
		"`brig network unpublish faker@nosuch --all`",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the notice does not say %q:\n%s", want, said)
		}
	}
	if got, _ := runtime.Publications("brig-faker-nosuch"); len(got) != 1 {
		t.Fatalf("the port was not recorded: %v", got)
	}
}

// A stopped sandbox is told apart from a missing one, and the notice names
// the run that publishes the port.
func TestPublishOntoAStoppedSandboxNamesTheRun(t *testing.T) {
	jsonRunHost(t, &existsRuntime{exists: true})
	t.Setenv("BRIG_GATEWAY_DIR", t.TempDir())
	said := publishNotice(t, "faker", "3000")
	if !strings.Contains(said, "faker is not running") ||
		!strings.Contains(said, "The next `brig run faker` publishes these ports") {
		t.Errorf("the notice does not name the run that publishes the port:\n%s", said)
	}
	if strings.Contains(said, "no sandbox") {
		t.Errorf("a stopped sandbox was reported as missing:\n%s", said)
	}
}
