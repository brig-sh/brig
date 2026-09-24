package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
)

// What each sandbox publishes, kept on disk.
//
// It has to be persisted, because a publication outlives the gateway that
// serves it. `brig stop` takes an isolated gateway with it and the shared one
// forgets a forward when it is replaced, so the set a sandbox asked for is
// brig's to remember and to re-apply on the next boot. It is also what the
// envelope reads, which is printed before any gateway is asked anything.
//
// Beside the gateway sockets and the two address maps, under BRIG_GATEWAY_DIR
// when that is set: this is the same bookkeeping, about the same network.

// publishedPath is the file, and publishedLock guards a read-modify-write of
// it.
func publishedPath() (string, error) {
	dir, err := gatewayDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "published.json"), nil
}

// readPublished is every sandbox's publications, keyed by sandbox name.
//
// A file that is not there is an empty map, which is the honest answer before
// anything has been published. Every other failure is an error.
//
// The distinction is what keeps one sandbox's trouble from reaching another.
// A read that fails, or a file half-written, used to read as "nobody publishes
// anything" -- and a write then persisted that guess, dropping every other
// sandbox's record. Their next boot reconciles against an empty set and
// withdraws forwards nobody asked to close.
func readPublished(path string) (map[string][]Publication, error) {
	all := map[string][]Publication{}
	blob, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return all, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read what these sandboxes publish (%s): %w", path, err)
	}
	if err := json.Unmarshal(blob, &all); err != nil {
		return nil, fmt.Errorf("%s is not readable as a publication record: %w. "+
			"Move it aside to start over; the sandboxes keep running, and each "+
			"republishes what it is next asked for", path, err)
	}
	return all, nil
}

// writePublished replaces the file, through a temporary and a rename so a
// crash mid-write cannot leave half a map behind.
func writePublished(path string, all map[string][]Publication) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	for name, list := range all {
		if len(list) == 0 {
			delete(all, name)
		}
	}
	blob, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".published-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Publications is what this sandbox is recorded as publishing, in the order a
// reader should meet them: by host port.
//
// A record that cannot be read is an error rather than an empty set. Reading
// it as empty is what would have a boot withdraw every port the sandbox has,
// on nothing worse than a transient failure to open a file.
func Publications(name string) ([]Publication, error) {
	path, err := publishedPath()
	if err != nil {
		return nil, err
	}
	all, err := readPublished(path)
	if err != nil {
		return nil, err
	}
	list := all[name]
	sort.Slice(list, func(i, j int) bool {
		if list[i].HostPort == list[j].HostPort {
			return list[i].Proto() < list[j].Proto()
		}
		return list[i].HostPort < list[j].HostPort
	})
	return list, nil
}

// RecordPublications adds these to what the sandbox already publishes, and
// returns the whole set.
//
// Adding rather than replacing. A publication is part of a sandbox's
// configuration and outlives one run of it, so `brig run` with no --publish on
// a sandbox that was published to must not silently withdraw the port. The
// envelope names every publication on every run, so nothing here is invisible;
// `brig network unpublish` is how one goes away.
//
// A repeat of one already recorded is not an error. Two --publish flags on one
// line that want the same host port are, and ParsePublications catches those
// before this is reached; the case here is the same command run twice.
func RecordPublications(name string, add []Publication) ([]Publication, error) {
	path, err := publishedPath()
	if err != nil {
		return nil, err
	}
	unlock, err := flock(path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	all, err := readPublished(path)
	if err != nil {
		return nil, err
	}
	have := all[name]
	for _, p := range add {
		replaced := false
		for i, q := range have {
			if q.Overlaps(p) {
				have[i], replaced = p, true
				break
			}
		}
		if !replaced {
			have = append(have, p)
		}
	}
	all[name] = have
	if err := writePublished(path, all); err != nil {
		return nil, fmt.Errorf("could not record what this sandbox publishes: %w", err)
	}
	return have, nil
}

// ForgetSomePublications drops these from the sandbox's record and returns
// what is left, with the ones that were actually there.
//
// The record alone. It is what a sandbox that is not running has -- there is
// no gateway to tell, and its next boot reconciles against this.
func ForgetSomePublications(name string, drop []Publication) (left, gone []Publication, err error) {
	path, err := publishedPath()
	if err != nil {
		return nil, nil, err
	}
	unlock, err := flock(path)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()

	all, err := readPublished(path)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range all[name] {
		if containsSame(drop, p) {
			gone = append(gone, p)
			continue
		}
		left = append(left, p)
	}
	all[name] = left
	if err := writePublished(path, all); err != nil {
		return nil, nil, fmt.Errorf("could not record what this sandbox publishes: %w", err)
	}
	return left, gone, nil
}

// ForgetPublications drops everything a removed sandbox published.
//
// Called from `brig rm` rather than from the adapter's Remove, which the run
// path also calls: a sandbox recreated because a share went stale is the same
// sandbox, and it has to come back offering the same ports.
//
// Best effort, like releasing an address: the record costs a few bytes and
// nothing about removing a sandbox depends on it. The forwards themselves go
// with the gateway that served them.
func ForgetPublications(name string) {
	path, err := publishedPath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readPublished(path)
	if err != nil {
		return
	}
	if _, ok := all[name]; !ok {
		return
	}
	delete(all, name)
	_ = writePublished(path, all)
}

// PrunePublications drops the record of every sandbox not in keep.
//
// `brig rm --all` calls it with the sandboxes that survived the removal. A
// record can also belong to a sandbox that never booted, because `brig network
// publish` records a port for one. Left behind, it would open that port on the
// next sandbox to take the name.
func PrunePublications(keep []string) {
	path, err := publishedPath()
	if err != nil {
		return
	}
	unlock, err := flock(path)
	if err != nil {
		return
	}
	defer unlock()
	all, err := readPublished(path)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, name := range keep {
		live[name] = true
	}
	pruned := false
	for name := range all {
		if !live[name] {
			delete(all, name)
			pruned = true
		}
	}
	if pruned {
		_ = writePublished(path, all)
	}
}

func containsSame(set []Publication, p Publication) bool {
	for _, q := range set {
		if q.Same(p) {
			return true
		}
	}
	return false
}

// sandboxAt is the sandbox holding a guest address, or "" when no record
// claims it.
//
// It is how a conflict names the sandbox in the way rather than only the
// address. Both allocators are consulted because the shared network and the
// isolated ones hand out addresses separately, and a conflict can be with
// either.
func sandboxAt(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	if alloc, err := sharedIPs(); err == nil {
		for name, host := range alloc.read() {
			if formatGatewayCIDR(host) == netip.PrefixFrom(addr, gatewayPrefix).String() {
				return name
			}
		}
	}
	if alloc, err := isolatedNets(); err == nil {
		for name, index := range alloc.read() {
			if sandboxCIDR(index) == netip.PrefixFrom(addr, sandboxNetBits).String() {
				return name
			}
		}
	}
	return ""
}
