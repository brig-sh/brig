package runtime

import (
	"encoding/json"
	"testing"
)

// Only an hvi block boot with no share of any kind owns its disk. Every other
// record is an ordinary sandbox or one this version cannot read, and neither
// may come out true: the caller refuses to recreate on true.
func TestOwnsItsDiskOnlyForABlockRootWithNoShare(t *testing.T) {
	record := func(id string, argv ...string) []byte {
		b, err := json.Marshal(map[string]any{"id": id, "backend": "hvi", "cmdLine": argv})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	const name = "brig-tp"
	for _, c := range []struct {
		what    string
		record  []byte
		want    bool
		wantErr bool
	}{
		{"a block root with no share",
			record(name, "/opt/hull/bin/hvi", "boot", "--kernel", "k", "--disk", "/s/rootfs.ext4", "--net-gateway", "/g"),
			true, false},
		{"a shared root",
			record(name, "hvi", "boot", "--kernel", "k", "--share-rw", "/s/rootfs", "rootfs", "--share-rw", "/h", "home"),
			false, false},
		{"a block root with the home shared in",
			record(name, "hvi", "boot", "--kernel", "k", "--disk", "/s/rootfs.ext4", "--share-rw", "/h", "home"),
			false, false},
		{"a block root with a read-only share",
			record(name, "hvi", "boot", "--kernel", "k", "--disk", "/s/rootfs.ext4", "--share-ro", "/h", "skills"),
			false, false},
		{"a share flag this version does not know",
			record(name, "hvi", "boot", "--kernel", "k", "--disk", "/s/rootfs.ext4", "--share-fd", "3"),
			false, false},
		{"no disk at all",
			record(name, "hvi", "boot", "--kernel", "k"),
			false, false},
		{"a --disk with no value",
			record(name, "hvi", "boot", "--kernel", "k", "--disk"),
			false, false},
		{"vz",
			[]byte(`{"id":"brig-tp","backend":"vz","cmdLine":["vz-runner","--rootfs","/s/rootfs.img"]}`),
			false, false},
		{"another instance's record", record("brig-other", "hvi", "boot", "--disk", "/d"), false, true},
		{"no cmdLine", []byte(`{"id":"brig-tp"}`), false, true},
		{"the smoke stub's record", []byte(`{"name": "brig-tp"}`), false, true},
		{"not JSON", []byte(`hull: oops`), false, true},
		{"a backend that disagrees",
			[]byte(`{"id":"brig-tp","backend":"vz","cmdLine":["hvi","boot","--disk","/d"]}`),
			false, true},
	} {
		got, err := ownsItsDisk(name, c.record)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: got %v, %v; want %v, error %v", c.what, got, err, c.want, c.wantErr)
		}
	}
}
