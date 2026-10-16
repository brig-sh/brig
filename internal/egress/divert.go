package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/divert"
)

// Diverts keeps a sandbox's divert sets in two places: in memory, where the
// relay checks each connection, and in the table, where the divert chain
// matches. An address goes into memory first. The relay then accepts every
// connection the table redirects.
type Diverts struct {
	// Set is the copy in memory.
	Set *divert.Set
	// Table is the sandbox's table.
	Table string
	// NFT is the command that runs nft. Nil changes the copy in memory alone.
	NFT []string

	// mu orders the adds against a reinstall of the table, so no add is
	// lost between the copy the reinstall takes and the new table.
	mu sync.Mutex
}

// add adds addrs to route i, in memory, and in the table when push is set.
func (d *Diverts) add(ctx context.Context, i int, addrs []netip.Addr, push bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	v4, err := d.Set.Add(i, addrs)
	if err != nil || !push || d.NFT == nil {
		return err
	}
	if script := DivertScript(d.Table, i, v4); script != "" {
		return RunNFT(ctx, d.NFT, script)
	}
	return nil
}

// Answered adds the addresses an answer gives each diverted host, before the
// guest gets the answer. The guest must not learn an address of a diverted
// host that the table would route directly.
func (d *Diverts) Answered(ctx context.Context, resp []byte) error {
	seen := map[string]bool{}
	for _, r := range d.Set.Routes() {
		if seen[r.Host] {
			continue
		}
		seen[r.Host] = true
		addrs, err := chainAddrs(resp, r.Host)
		if err != nil {
			return err
		}
		if len(addrs) == 0 {
			continue
		}
		for _, i := range d.Set.Lookup(r.Host) {
			if err := d.add(ctx, i, addrs, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// script returns the nft script that puts every address in memory in the
// table. The caller holds mu.
func (d *Diverts) script() string {
	var b strings.Builder
	for i := range d.Set.Routes() {
		b.WriteString(DivertScript(d.Table, i, d.Set.Addrs(i)))
	}
	return b.String()
}

// install runs the table's script with every address in memory, in one
// transaction. A table installed again then diverts what it diverted
// before.
func (d *Diverts) install(ctx context.Context, nft []string, script string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return RunNFT(ctx, nft, script+d.script())
}

// Resolve resolves the host of each route through upstream and adds what it
// returns. push says whether the table exists yet. It fails on a host with
// no IPv4 address.
func (d *Diverts) Resolve(ctx context.Context, res *net.Resolver, push bool) error {
	var errs []error
	for i, r := range d.Set.Routes() {
		if _, err := netip.ParseAddr(r.Host); err == nil {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
		addrs, err := res.LookupNetIP(lctx, "ip4", r.Host)
		cancel()
		if err == nil && len(addrs) == 0 {
			err = errors.New("no IPv4 address")
		}
		if err == nil {
			err = d.add(ctx, i, addrs, push)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("divert %s: %w", r, err))
		}
	}
	return errors.Join(errs...)
}

// watch resolves the routes' hosts again every interval until ctx is done.
// A failure keeps the addresses already in the sets.
func (d *Diverts) watch(ctx context.Context, res *net.Resolver, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.Resolve(ctx, res, true); err != nil && ctx.Err() == nil {
				logf("egress: %v", err)
			}
		}
	}
}
