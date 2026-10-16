package divert

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseRoute(t *testing.T) {
	for in, want := range map[string]Route{
		"api.anthropic.com:443":   {"api.anthropic.com", 443},
		"API.Anthropic.COM.:443":  {"api.anthropic.com", 443},
		"160.79.104.10:8443":      {"160.79.104.10", 8443},
		"[::ffff:1.2.3.4]:443":    {"1.2.3.4", 443},
		"host_with-dash.test:1":   {"host_with-dash.test", 1},
		"a.b.c.d.example.org:443": {"a.b.c.d.example.org", 443},
	} {
		got, err := ParseRoute(in)
		if err != nil || got != want {
			t.Errorf("ParseRoute(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for in, why := range map[string]string{
		"api.anthropic.com":       "is not host:port",
		"api.anthropic.com:0":     "is not a TCP port",
		"api.anthropic.com:65536": "is not a TCP port",
		"api.anthropic.com:https": "is not a TCP port",
		"resolver.test:53":        "port 53",
		"[2001:db8::1]:443":       "no IPv6",
		"a b.test:443":            "is not a host name",
		"a..test:443":             "is not a host name",
		"a/b.test:443":            "is not a host name",
		":443":                    "is not a host name",
	} {
		if _, err := ParseRoute(in); err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("ParseRoute(%q) = %v; want an error naming %q", in, err, why)
		}
	}
}

func TestPreambleIsOneExactLine(t *testing.T) {
	got, err := Preamble(netip.MustParseAddrPort("160.79.104.10:443"), netip.MustParseAddrPort("10.4.1.2:51234"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "BRIG-DIVERT/1 160.79.104.10 443 10.4.1.2 51234\n"; string(got) != want {
		t.Errorf("Preamble = %q, want %q", got, want)
	}
}

// IPv6 is written without brackets or a zone, and an IPv4 address mapped into
// IPv6 as plain IPv4, which is what netip.Addr.String gives for each.
func TestPreambleWritesTheContractsAddressForm(t *testing.T) {
	dst := netip.AddrPortFrom(netip.MustParseAddr("fe80::1%eth0"), 443)
	src := netip.MustParseAddrPort("[::ffff:10.4.1.2]:40000")
	got, err := Preamble(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	if want := "BRIG-DIVERT/1 fe80::1 443 10.4.1.2 40000\n"; string(got) != want {
		t.Errorf("Preamble = %q, want %q", got, want)
	}
	// The longest preamble, two full IPv6 addresses, fits.
	long := netip.MustParseAddr("ffff:ffff:ffff:ffff:ffff:ffff:ffff:fffe")
	got, err = Preamble(netip.AddrPortFrom(long, 65535), netip.AddrPortFrom(long, 65535))
	if err != nil || len(got) > MaxPreamble {
		t.Errorf("the longest preamble is %d bytes: %v", len(got), err)
	}
	if _, err := Preamble(netip.AddrPort{}, src); err == nil {
		t.Error("Preamble accepted an empty destination")
	}
	if _, err := Preamble(netip.AddrPortFrom(long, 0), src); err == nil {
		t.Error("Preamble accepted a destination on port 0, which ParsePreamble refuses")
	}
}

func TestParsePreambleReadsWhatPreambleWrites(t *testing.T) {
	for _, c := range [][2]string{
		{"160.79.104.10:443", "10.4.1.2:51234"},
		{"[2001:db8::1]:443", "[2001:db8::2]:1"},
	} {
		dst, src := netip.MustParseAddrPort(c[0]), netip.MustParseAddrPort(c[1])
		line, err := Preamble(dst, src)
		if err != nil {
			t.Fatal(err)
		}
		gd, gs, err := ParsePreamble(line)
		if err != nil || gd != dst || gs != src {
			t.Errorf("ParsePreamble(%q) = %s %s %v", line, gd, gs, err)
		}
	}
}

func TestParsePreambleRefusesWhatPreambleNeverWrites(t *testing.T) {
	for _, line := range []string{
		"BRIG-DIVERT/1 1.2.3.4 443 10.4.1.2 5",
		"BRIG-DIVERT/2 1.2.3.4 443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 1.2.3.4 443 10.4.1.2 5 extra\n",
		"BRIG-DIVERT/1  1.2.3.4 443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 1.2.3.4 0443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 1.2.3.4 65536 10.4.1.2 5\n",
		// No connection goes to or comes from port 0.
		"BRIG-DIVERT/1 1.2.3.4 0 10.4.1.2 5\n",
		"BRIG-DIVERT/1 1.2.3.4 443 10.4.1.2 0\n",
		"BRIG-DIVERT/1 [2001:db8::1] 443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 2001:DB8::1 443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 fe80::1%eth0 443 10.4.1.2 5\n",
		"BRIG-DIVERT/1 1.2.3.4 443 10.4.1.2 5\r\n",
		"BRIG-DIVERT/1 1.2.3.4 443 10.4.1.2 " + strings.Repeat("5", 200) + "\n",
	} {
		if _, _, err := ParsePreamble([]byte(line)); err == nil {
			t.Errorf("ParsePreamble accepted %q", line)
		}
	}
}

func TestSetMatchesAddressAndPort(t *testing.T) {
	s := NewSet([]Route{{"api.anthropic.com", 443}, {"api.anthropic.com", 8443}, {"203.0.113.9", 443}})
	if _, err := s.Add(0, []netip.Addr{netip.MustParseAddr("160.79.104.10"), netip.MustParseAddr("2001:db8::1")}); err != nil {
		t.Fatal(err)
	}
	for dst, want := range map[string]bool{
		"160.79.104.10:443":          true,
		"160.79.104.10:8443":         false, // route 1 has no address yet
		"160.79.104.10:80":           false,
		"160.79.104.11:443":          false,
		"203.0.113.9:443":            true, // a literal route starts with its address
		"203.0.113.9:8443":           false,
		"[2001:db8::1]:443":          false, // IPv6 never joins a set
		"[::ffff:160.79.104.10]:443": true,
	} {
		if got := s.Match(netip.MustParseAddrPort(dst)); got != want {
			t.Errorf("Match(%s) = %v, want %v", dst, got, want)
		}
	}
	if got := s.Lookup("API.anthropic.com."); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Errorf("Lookup = %v", got)
	}
	if got := s.Lookup("anthropic.com"); len(got) != 0 {
		t.Errorf("Lookup of another name = %v", got)
	}
	if _, err := s.Add(3, nil); err == nil {
		t.Error("Add accepted a route that does not exist")
	}
}

// An address never leaves a set while the relay runs. A full set refuses the
// whole add, so the resolver fails the query and the guest never learns an
// address the table would route directly.
func TestSetIsBounded(t *testing.T) {
	s := NewSet([]Route{{"many.test", 443}})
	var addrs []netip.Addr
	a := netip.MustParseAddr("10.0.0.0")
	for range MaxAddrs - 1 {
		a = a.Next()
		addrs = append(addrs, a)
	}
	if _, err := s.Add(0, addrs); err != nil {
		t.Fatal(err)
	}
	// Addresses already in the set do not count again.
	if _, err := s.Add(0, addrs[:10]); err != nil {
		t.Errorf("re-adding known addresses failed: %v", err)
	}
	if _, err := s.Add(0, []netip.Addr{a.Next(), a.Next().Next()}); err == nil {
		t.Error("Add went past MaxAddrs")
	}
	if s.Match(netip.AddrPortFrom(a.Next(), 443)) {
		t.Error("a refused add left an address in the set")
	}
	if _, err := s.Add(0, []netip.Addr{a.Next()}); err != nil {
		t.Errorf("the last free slot was refused: %v", err)
	}
	if n := len(s.Addrs(0)); n != MaxAddrs {
		t.Errorf("the set holds %d addresses", n)
	}
}
