package runtime

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubHolder stands in for lsof, so a test does not depend on what the host
// has installed or on who is listening on it.
func stubHolder(t *testing.T, holder string) {
	t.Helper()
	prev := portHolder
	portHolder = func(Publication) string { return holder }
	t.Cleanup(func() { portHolder = prev })
}

// isolatedGatewayAt stands up the gateway of an isolated sandbox beside the
// shared one sharedGatewayAt made, with the address map that names it.
func isolatedGatewayAt(t *testing.T, sandbox string, index int) *forwardAPI {
	t.Helper()
	sock, err := isolatedSocket(sandbox)
	if err != nil {
		t.Fatalf("isolatedSocket: %v", err)
	}
	g := &forwardAPI{sock: sock}
	api, err := net.Listen("unix", gatewayAPISocket(sock))
	if err != nil {
		t.Fatalf("listen on the api socket: %v", err)
	}
	srv := &http.Server{Handler: g, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(api) }()
	t.Cleanup(func() { _ = srv.Close() })
	qemu, err := net.Listen("unix", qemuGatewaySocket(sock))
	if err != nil {
		t.Fatalf("listen on the qemu socket: %v", err)
	}
	t.Cleanup(func() { _ = qemu.Close() })
	alloc, err := isolatedNets()
	if err != nil {
		t.Fatalf("isolatedNets: %v", err)
	}
	if err := alloc.write(map[string]int{sandbox: index}); err != nil {
		t.Fatalf("write the network map: %v", err)
	}
	return g
}

// The stress test's case. nc held 127.0.0.1:18090, and publishing there was
// reported as "already published", which no sandbox was. The gateway refuses
// a bind the host would not give it with a conflict that names no guest.
func TestPublishNamesAProcessHoldingTheHostPort(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	g.outside = map[string]bool{"127.0.0.1:18090": true}
	stubHolder(t, "nc, pid 951")

	h := &hull{bin: "hull"}
	p, _ := ParsePublication("18090:8000")
	err := h.Publish(sandbox, p)
	if err == nil {
		t.Fatal("publishing on a port another process holds was allowed")
	}
	if strings.Contains(err.Error(), "already published") {
		t.Errorf("a port no sandbox publishes was reported as published: %v", err)
	}
	for _, want := range []string{"in use by another process (nc, pid 951)", "`18091:8000`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if rec := published(t, sandbox); len(rec) != 0 {
		t.Fatalf("a refused publish was recorded: %+v", rec)
	}
}

// lsof can be missing, and it cannot see another user's process. The refusal
// still says that a process holds the port.
func TestAnUnnamedHolderIsStillAnotherProcess(t *testing.T) {
	const sandbox = "brig-claude-web"
	g := sharedGatewayAt(t, sandbox, 5)
	g.outside = map[string]bool{"127.0.0.1:18090": true}
	stubHolder(t, "")

	p, _ := ParsePublication("18090:8000")
	err := (&hull{bin: "hull"}).Publish(sandbox, p)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:18090 is in use by another process on this host") {
		t.Fatalf("err = %v, want the port named as held by another process", err)
	}
}

// An isolated sandbox publishes on a gateway of its own, so the shared gateway
// finds that port held from outside. The holder is still a sandbox, and the
// refusal names it.
func TestPublishNamesASandboxOnAnotherGateway(t *testing.T) {
	const sandbox, other = "brig-claude-web", "brig-claude-iso"
	g := sharedGatewayAt(t, sandbox, 5)
	g.outside = map[string]bool{"127.0.0.1:18080": true}
	iso := isolatedGatewayAt(t, other, 3)
	iso.forwards = []gatewayForward{{
		Protocol: "tcp", Local: "127.0.0.1:18080", Remote: addrOf(sandboxCIDR(3)) + ":80",
	}}
	stubHolder(t, "hull, pid 4242")

	p, _ := ParsePublication("18080:3000")
	err := (&hull{bin: "hull"}).Publish(sandbox, p)
	if err == nil || !strings.Contains(err.Error(), "already published by "+other) {
		t.Fatalf("err = %v, want the isolated sandbox named", err)
	}
}

func TestLsofHolderReadsTheFirstProcess(t *testing.T) {
	for out, want := range map[string]string{
		"p44598\ncnc\nf3\nn127.0.0.1:18190\n": "nc, pid 44598",
		"p10\ncpython3\nf3\np11\ncnode\nf4\n": "python3, pid 10",
		"":                                    "",
		"lsof: WARNING: can't stat() nfs file system": "",
	} {
		if got := lsofHolder(out); got != want {
			t.Errorf("lsofHolder(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestHostPortHeldSeesAListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := Publication{HostPort: l.Addr().(*net.TCPAddr).Port, GuestPort: 80}
	if !hostPortHeld(p) {
		t.Errorf("%s is listened on and read as free", p.Local())
	}
	_ = l.Close()
	if hostPortHeld(p) {
		t.Errorf("%s is free and read as held", p.Local())
	}
}

func TestMappedPortsReadsThePortsColumn(t *testing.T) {
	got := mappedPorts("127.0.0.1:18080->8000/tcp, 0.0.0.0:5353->53/udp, 0.0.0.0:9000-9001->9000-9001/tcp")
	want := []Publication{
		{HostAddr: "127.0.0.1", HostPort: 18080, GuestPort: 8000},
		{HostAddr: "0.0.0.0", HostPort: 5353, GuestPort: 53, Protocol: "udp"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mappedPorts = %+v, want %+v", got, want)
	}
	if got := mappedPorts(""); len(got) != 0 {
		t.Errorf("an empty column read as %+v", got)
	}
}

// refusingNerdctl is a nerdctl whose run refuses the way nerdctl refuses a
// held host port, and whose ps prints ps.
func refusingNerdctl(t *testing.T, ps string) *nerdctl {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps.out"), []byte(ps), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "nerdctl")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  ps) cat '" + filepath.Join(dir, "ps.out") + "' ;;\n" +
		"  run) echo 'level=fatal msg=\"failed to load networking flags: port is already allocated\"' >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &nerdctl{bin: bin}
}

// On Linux nerdctl refuses a held port itself, as "port is already
// allocated". brig names the holder there too: another sandbox when a
// container maps the port, and a process when none does.
func TestNerdctlNamesWhatHoldsAHostPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	p, _ := ParsePublication(port + ":8000")
	stubHolder(t, "python3, pid 931516")
	spec := RunSpec{Name: "brig-ubuntu-b", Image: "img", Mem: 1, CPUs: 1, Publish: []Publication{p}}

	n := refusingNerdctl(t, "brig-ubuntu-a\tUp\t127.0.0.1:"+port+"->8000/tcp\n")
	err = n.Run(spec)
	if err == nil || !strings.Contains(err.Error(), "already published by brig-ubuntu-a") ||
		!strings.Contains(err.Error(), "`brig rm`") {
		t.Errorf("a port another container maps: err = %v, want that sandbox named", err)
	}

	n = refusingNerdctl(t, "brig-ubuntu-c\tUp\t127.0.0.1:1->8000/tcp\n")
	err = n.Run(spec)
	if err == nil || !strings.Contains(err.Error(), "in use by another process (python3, pid 931516)") {
		t.Errorf("a port no container maps: err = %v, want the process named", err)
	}
}

// A run that fails for another reason, with every host port free, keeps
// nerdctl's own words.
func TestNerdctlKeepsItsOwnRefusalWhenThePortsAreFree(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	stubHolder(t, "python3, pid 931516")
	n := refusingNerdctl(t, "")
	err = n.Run(RunSpec{Name: "brig-ubuntu-b", Image: "img", Mem: 1, CPUs: 1,
		Publish: []Publication{{HostPort: port, GuestPort: 8000}}})
	if err == nil || !strings.Contains(err.Error(), "already allocated") ||
		strings.Contains(err.Error(), "in use by") {
		t.Errorf("err = %v, want nerdctl's own refusal", err)
	}
}

// The real lsof, where the host has one, names this test as the holder of a
// port it listens on. An lsof that answers nothing in time names nothing,
// which is the answer the refusal falls back on.
func TestPortHolderNamesTheListener(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("no lsof on this host")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	p := Publication{HostPort: l.Addr().(*net.TCPAddr).Port, GuestPort: 80}
	got := portHolder(p)
	if got == "" {
		t.Skip("lsof named nothing within the timeout on this host")
	}
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.HasSuffix(got, want) {
		t.Errorf("portHolder = %q, want this process (%s)", got, want)
	}
}
