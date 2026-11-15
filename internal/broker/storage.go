package broker

import (
	"net/url"
	"strings"
)

// storageObjects returns the segments of escapedRest, the path after a
// route with services, that name a Cloud Storage object in the JSON API:
// the segment after b/<bucket>/o/ under /storage/v1/ or
// /download/storage/v1/, and the destination object of rewriteTo, copyTo
// and moveTo. The keys index strings.Split(escapedRest[1:], "/"), whose
// first segment is the service.
//
// The JSON API takes an object name as one segment, with its slashes
// escaped, and does not read %2F there as a separator. These are the only
// segments where the broker takes an escaped slash. Other Google APIs may
// decode one before they route the request, and serve a path that
// request.allow did not match. Each segment of the grammar is compared as
// the guest escaped it, so an escaped b or o is not that segment.
func storageObjects(escapedRest string) map[int]bool {
	segs := strings.Split(strings.TrimPrefix(escapedRest, "/"), "/")
	if len(segs) == 0 || segs[0] != "storage" {
		return nil
	}
	at := func(i int, want ...string) bool {
		if i+len(want) > len(segs) {
			return false
		}
		for j, w := range want {
			if segs[i+j] != w {
				return false
			}
		}
		return true
	}
	// k is the index of b in /storage/v1/b/<bucket>/o/<object>.
	k := 3
	if at(1, "download") {
		k = 4
	}
	if !at(k-2, "storage", "v1", "b") || !at(k+2, "o") || k+3 >= len(segs) {
		return nil
	}
	objects := map[int]bool{k + 3: true}
	switch {
	case (at(k+4, "rewriteTo", "b") || at(k+4, "copyTo", "b")) && at(k+7, "o") && k+8 < len(segs):
		objects[k+8] = true
	case at(k+4, "moveTo", "o") && k+6 < len(segs):
		objects[k+6] = true
	}
	return objects
}

// matchPath is rest, the path after the route, as request.allow matches it:
// decoded, with the slashes of an object name escaped as %2F, so that * in
// /storage/v1/b/*/o/* matches the whole name. objects are the segments of
// erest, the same path as the guest escaped it, that name an object; with
// none it is rest.
func matchPath(rest, erest string, objects map[int]bool) string {
	if len(objects) == 0 {
		return rest
	}
	segs := strings.Split(strings.TrimPrefix(erest, "/"), "/")
	for i, seg := range segs {
		// canonical has checked that every escape decodes.
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return rest
		}
		if objects[i] {
			dec = strings.ReplaceAll(dec, "/", "%2F")
		}
		segs[i] = dec
	}
	return "/" + strings.Join(segs, "/")
}
