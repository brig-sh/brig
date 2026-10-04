package telemetry

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	hull "github.com/brig-sh/hull/pkg/telemetry"
)

// home gives a test a home of its own and clears the opt-out variables the
// machine running it may have set.
func home(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	for _, v := range []string{"DO_NOT_TRACK", "HULL_TELEMETRY_DISABLED", "HULL_TELEMETRY_DEBUG",
		"HULL_TELEMETRY_SUPPRESS", "HULL_TELEMETRY_ENDPOINT", "HULL_TELEMETRY_PRODUCT", "HULL_TELEMETRY_VERSION"} {
		t.Setenv(v, "")
	}
	// A build with no endpoint queues nothing. Nothing listens on this one.
	t.Setenv("HULL_TELEMETRY_ENDPOINT", "http://127.0.0.1:1")
	t.Cleanup(func() { state.client = nil })
	return dir
}

// collector is an endpoint that records every body it receives.
func collector(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		mu.Lock()
		bodies = append(bodies, b.String())
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HULL_TELEMETRY_ENDPOINT", srv.URL)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// The answer is hull's, in hull's store: brig keeps no second copy that could
// disagree with the one hull reads.
func TestSetTelemetryRecordsTheAnswer(t *testing.T) {
	dir := home(t)
	store := filepath.Join(dir, ".hull")

	if err := Set(false); err != nil {
		t.Fatalf("off: %v", err)
	}
	if answer, _, _ := Status(); answer != Off {
		t.Errorf("after off, brig reads %q", answer)
	}
	if answer, _ := hull.Effective(store); answer != hull.Off {
		t.Errorf("after off, hull reads %v", answer)
	}
	if err := Set(true); err != nil {
		t.Fatalf("on: %v", err)
	}
	if answer, _, _ := Status(); answer != On {
		t.Errorf("after on, brig reads %q", answer)
	}
	if answer, _ := hull.Effective(store); answer != hull.On {
		t.Errorf("after on, hull reads %v", answer)
	}
}

// A profile name of the user's own can say anything about them, so it never
// leaves the machine. What goes out is "custom" and a hash that is the same for
// the same name everywhere, so it can still be counted.
func TestCustomAgentIsSentAsAHash(t *testing.T) {
	agent, hash := AgentFields("claude-code", true)
	if agent != "claude-code" || hash != "" {
		t.Errorf("a built-in profile was sent as %q, %q", agent, hash)
	}
	agent, hash = AgentFields("acme-internal-reviewer", false)
	if agent != Custom {
		t.Errorf("a custom profile was sent as %q", agent)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(hash) {
		t.Errorf("agent_hash = %q, want 16 hex digits", hash)
	}
	if _, again := AgentFields("acme-internal-reviewer", false); again != hash {
		t.Error("the same name hashed differently, so it cannot be counted")
	}
	if _, other := AgentFields("acme-internal-writer", false); other == hash {
		t.Error("two names hashed the same")
	}
}

func TestCommandEventCarriesBrigsFields(t *testing.T) {
	home(t)
	received := collector(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}

	Start("run", false)
	SetAgent("acme-internal-reviewer", false)
	SetRuntime("hull")
	Finish("error", "credentials")
	Upload()

	got := received()
	if len(got) != 1 {
		t.Fatalf("collector received %d events, want 1: %q", len(got), got)
	}
	for _, want := range []string{
		`"event":"command"`, `"product":"brig"`, `"command":"run"`,
		`"agent":"custom"`, `"agent_hash":"`, `"runtime":"hull"`,
		`"outcome":"error"`, `"error_class":"credentials"`,
		`"platform":"` + hull.Platform() + `"`,
		`"version":"` + Version() + `"`,
	} {
		if !strings.Contains(got[0], want) {
			t.Errorf("event lacks %s:\n%s", want, got[0])
		}
	}
	if strings.Contains(got[0], "acme-internal-reviewer") {
		t.Errorf("a custom profile's name left the machine:\n%s", got[0])
	}
	if strings.Contains(got[0], "runtime_version") {
		t.Errorf("brig's own event carries hull's version field:\n%s", got[0])
	}
}

// brig keeps its rule from before it sent anything itself: an install nobody
// has asked sends nothing from a script, a pipe or CI.
func TestUnansweredUnattendedRunSendsNothing(t *testing.T) {
	dir := home(t)
	received := collector(t)

	Start("run", false)
	if Counting() {
		t.Error("an unanswered install is counting")
	}
	Finish("ok", "")
	Upload()

	if got := received(); len(got) != 0 {
		t.Errorf("an unanswered install sent %q", got)
	}
	if answer, _ := hull.Effective(filepath.Join(dir, ".hull")); answer != hull.Unanswered {
		t.Errorf("staying off recorded an answer: %v", answer)
	}
}

// The exit path may run more than once. The run counts once.
func TestFinishSendsOnce(t *testing.T) {
	home(t)
	received := collector(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}

	Start("sh", false)
	Finish("ok", "")
	Finish("error", "other")
	Upload()

	if got := received(); len(got) != 1 || !strings.Contains(got[0], `"outcome":"ok"`) {
		t.Errorf("want one ok event, got %q", got)
	}
}

// A panic message can carry a path or a profile name, so the report carries
// the panic's type and its stack only.
func TestPanicIsQueuedWithoutItsMessage(t *testing.T) {
	dir := home(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	spawned := spawns(t)
	Start("run", false)

	CapturePanic(errors.New("open /Users/someone/acme-secret: no such file"), []byte("goroutine 1 [running]:\nmain.main()\n"))
	if *spawned != 1 {
		t.Errorf("the crash report started the uploader %d times, want once", *spawned)
	}

	files, _ := filepath.Glob(filepath.Join(dir, ".hull", "crashes", "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one queued report, got %v", files)
	}
	body, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"product":"brig"`) || !strings.Contains(string(body), `"panic_type":"*errors.errorString"`) {
		t.Errorf("the report is not brig's panic:\n%s", body)
	}
	if strings.Contains(string(body), "acme-secret") {
		t.Errorf("the panic message was queued:\n%s", body)
	}
}

// Without a home there is no store to read an answer from, and a store made
// in whatever directory brig runs in would be a file left in someone's
// project. Nothing is read, written or sent.
func TestNoHomeSendsNothing(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("HOME", "")
	t.Cleanup(func() { state.client = nil })

	Start("run", true)
	if Counting() {
		t.Error("counting with no home")
	}
	if _, _, err := Status(); err == nil {
		t.Error("status answered with no home")
	}
	if entries, _ := os.ReadDir(cwd); len(entries) != 0 {
		t.Errorf("files written to the working directory: %v", entries)
	}
}

// brigd reads the answer so the sandboxes it boots report through hull, and
// it has no command of its own to count.
func TestDaemonSendsNoCommandEvent(t *testing.T) {
	home(t)
	received := collector(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}

	Start("", false)
	if !Counting() {
		t.Error("an answered install is not counting under brigd")
	}
	Finish("ok", "")
	Upload()

	if got := received(); len(got) != 0 {
		t.Errorf("brigd sent %q", got)
	}
}

// The handover replaces brig's process, and the agent's terminal must not
// wait on the network: the event is queued for the session to send.
func TestHandoverQueuesTheEventAndSendsNothing(t *testing.T) {
	dir := home(t)
	received := collector(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	spawned := spawns(t)
	Start("run", false)

	Handover()
	Finish("error", "other")
	if *spawned != 1 {
		t.Errorf("the handover started the uploader %d times, want once", *spawned)
	}

	if got := received(); len(got) != 0 {
		t.Fatalf("the handover sent %q", got)
	}
	files, _ := filepath.Glob(filepath.Join(dir, ".hull", "outbox", "*.json"))
	if len(files) != 1 {
		t.Fatalf("want the one handover event queued, got %v", files)
	}
	body, _ := os.ReadFile(files[0])
	if !strings.Contains(string(body), `"outcome":"ok"`) {
		t.Fatalf("the queued event is not the handover's: %s", body)
	}
}

// Under sudo with the caller's HOME, files brig made there would belong to
// root, and the user's own answer could no longer be written.
func TestHomeOfAnotherUserGetsNoFiles(t *testing.T) {
	dir := home(t)
	prev := homeOwner
	homeOwner = func(string) (int, bool) { return os.Geteuid() + 1, true }
	t.Cleanup(func() { homeOwner = prev })

	Start("ls", false)
	// The message says why, so that the user knows to drop the sudo.
	if _, _, err := Status(); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("status in a home that belongs to someone else: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".hull")); !os.IsNotExist(err) {
		t.Errorf("brig wrote into a home that belongs to someone else: %v", err)
	}
}

// spawns makes SpawnUploader count its calls for the test.
func spawns(t *testing.T) *int {
	t.Helper()
	n := 0
	prev := SpawnUploader
	SpawnUploader = func() { n++ }
	t.Cleanup(func() { SpawnUploader = prev })
	return &n
}

// slowCollector is a collector that takes delay to answer each request, the
// way a real one does across the network.
func slowCollector(t *testing.T, delay time.Duration) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		mu.Lock()
		n++
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HULL_TELEMETRY_ENDPOINT", srv.URL)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// A short command ends before an upload over a real network can. An upload
// started in its own process would be cut off and leave the event claimed, so
// the command only queues it and hands the upload to another process.
func TestACommandQueuesAndLeavesTheUploadToAnotherProcess(t *testing.T) {
	dir := home(t)
	slowCollector(t, 2*time.Second)
	spawned := spawns(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}

	// An event an earlier command left queued.
	Start("ps", false)
	Finish("ok", "")

	begin := time.Now()
	Start("ls", false)
	Finish("ok", "")
	took := time.Since(begin)

	if took > 200*time.Millisecond {
		t.Errorf("the command waited %v on the network", took)
	}
	if *spawned != 2 {
		t.Errorf("the uploader was started %d times, want once per command", *spawned)
	}
	if claimed, _ := filepath.Glob(filepath.Join(dir, ".hull", "outbox", "*.uploading")); len(claimed) != 0 {
		t.Errorf("the command left an upload in flight: %v", claimed)
	}
	if queued, _ := filepath.Glob(filepath.Join(dir, ".hull", "outbox", "*.json")); len(queued) != 2 {
		t.Errorf("want both events queued for the uploader, got %v", queued)
	}
}

// The uploader waits for a slow collector, and sends the whole queue.
func TestUploadWaitsForASlowCollector(t *testing.T) {
	dir := home(t)
	received := slowCollector(t, 400*time.Millisecond)
	spawns(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"ls", "ps", "stop", "rm", "run"} {
		Start(verb, false)
		Finish("ok", "")
	}

	Upload()

	if got := received(); got != 5 {
		t.Errorf("the collector received %d events, want 5", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".hull", "outbox", "*")); len(left) != 0 {
		t.Errorf("the outbox still holds %v", left)
	}
}

// Nothing queued, nothing to upload: no process is started for it.
func TestNothingQueuedStartsNoUploader(t *testing.T) {
	home(t)
	spawned := spawns(t)

	Start("ls", false)
	Finish("ok", "")

	if *spawned != 0 {
		t.Errorf("an install nobody answered started the uploader %d times", *spawned)
	}
}

// One upload at a time: a second one finds the lock held and leaves the queue
// to the first.
func TestUploadLeavesTheQueueToTheOneRunning(t *testing.T) {
	dir := home(t)
	received := collector(t)
	spawns(t)
	if err := Set(true); err != nil {
		t.Fatal(err)
	}
	Start("ls", false)
	Finish("ok", "")

	lock, err := os.OpenFile(filepath.Join(dir, ".hull", uploadLock), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	Upload()
	if got := received(); len(got) != 0 {
		t.Errorf("an upload ran beside the one holding the lock: %q", got)
	}

	_ = lock.Close()
	Upload()
	if got := received(); len(got) != 1 {
		t.Errorf("after the lock was released, the collector received %d events, want 1", len(got))
	}
}
