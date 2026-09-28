package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// bundleManifest is the manifest shape hull-assets publishes: one layer per
// file, named by its title annotation, and the file's own sha256 as the layer
// digest.
func bundleManifest(mediaType string, files map[string]string) []byte {
	var layers []string
	for name, digest := range files {
		layers = append(layers, fmt.Sprintf(`{"mediaType":"application/octet-stream","digest":%q,"size":1,`+
			`"annotations":{"org.opencontainers.image.title":%q}}`, digest, name))
	}
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"artifactType":"application/vnd.nofire.hull-assets.v1",`+
		`"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},`+
		`"layers":[%s]}`, mediaType, strings.Join(layers, ",")))
}

const ociManifest = "application/vnd.oci.image.manifest.v1+json"

var (
	kernelDigest = sha([]byte("kernel"))
	initrdDigest = sha([]byte("initrd"))
)

// fakeRegistry serves one manifest at /v2/nofireai/hull-assets/manifests/<served>
// behind the anonymous token flow ghcr uses. It records the paths asked for.
type fakeRegistry struct {
	srv      *httptest.Server
	body     []byte
	served   string
	realm    string
	redirect string
	asked    []string
	tokenHit int
}

func newFakeRegistry(t *testing.T, body []byte, served string) *fakeRegistry {
	t.Helper()
	r := &fakeRegistry{body: body, served: served}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.asked = append(r.asked, req.URL.Path)
		switch {
		case req.URL.Path == "/redirect":
			http.Redirect(w, req, r.redirect, http.StatusFound)
		case req.URL.Path == "/token":
			r.tokenHit++
			if req.URL.Query().Get("scope") != "repository:nofireai/hull-assets:pull" {
				http.Error(w, "wrong scope", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case req.URL.Path == "/v2/nofireai/hull-assets/manifests/"+r.served:
			if req.Header.Get("Authorization") != "Bearer anon" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(
					`Bearer realm=%q,service="test",scope="repository:nofireai/hull-assets:pull"`, r.realm))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", ociManifest)
			_, _ = w.Write(r.body)
		default:
			http.NotFound(w, req)
		}
	}))
	r.realm = r.srv.URL + "/token"
	t.Cleanup(r.srv.Close)
	was := registryClient
	registryClient = r.srv.Client()
	t.Cleanup(func() { registryClient = was })
	return r
}

func (r *fakeRegistry) ref(tag string) string {
	return strings.TrimPrefix(r.srv.URL, "https://") + "/nofireai/hull-assets:" + tag
}

// The manifest is fetched by the digest that verified, never by the tag, and
// through the anonymous token flow, so what it lists is what was signed.
func TestBundleDigestsReadsTheManifestByTheVerifiedDigest(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest, "container-initrd": initrdDigest})
	reg := newFakeRegistry(t, body, sha(body))

	got, err := BundleDigests(reg.ref("darwin-arm64"), sha(body))
	if err != nil {
		t.Fatalf("a manifest served at its own digest was not read: %v", err)
	}
	if got["Image"] != kernelDigest || got["container-initrd"] != initrdDigest {
		t.Errorf("digests = %v, want the two layers the manifest lists", got)
	}
	if reg.tokenHit != 1 {
		t.Errorf("the token endpoint was asked %d times, want once", reg.tokenHit)
	}
	for _, p := range reg.asked {
		if strings.Contains(p, "darwin-arm64") {
			t.Errorf("the manifest was fetched by tag: %s", p)
		}
	}
}

// A registry that answers the digest with other bytes is not serving that
// manifest. Its layer list names files nobody signed.
func TestBundleDigestsRefusesAManifestThatIsNotTheDigest(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest, "container-initrd": initrdDigest})
	asked := sha([]byte("the manifest that verified"))
	reg := newFakeRegistry(t, body, asked)

	got, err := BundleDigests(reg.ref("darwin-arm64"), asked)
	if err == nil || !strings.Contains(err.Error(), "whose digest is") {
		t.Fatalf("read digests %v from a manifest whose bytes are not %s (err %v)", got, asked, err)
	}
	var refused *ManifestRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("the registry answered with other bytes, and the error does not say it answered: %T %v", err, err)
	}
}

// An index lists manifests rather than files, so no layer in it is a kernel.
func TestBundleDigestsRefusesAnIndex(t *testing.T) {
	body := bundleManifest("application/vnd.oci.image.index.v1+json", map[string]string{"Image": kernelDigest})
	reg := newFakeRegistry(t, body, sha(body))

	got, err := BundleDigests(reg.ref("darwin-arm64"), sha(body))
	if err == nil || !strings.Contains(err.Error(), "not an image manifest") {
		t.Fatalf("read digests %v from an index (err %v)", got, err)
	}
	var refused *ManifestRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("an index is an answer brig refused, and the error does not say so: %T %v", err, err)
	}
}

// A realm over plain http carries the token request in the clear, and
// the realm is whatever the registry's answer says. It is refused before any
// request goes to it.
func TestBundleDigestsRefusesAPlainHTTPRealm(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest})
	reg := newFakeRegistry(t, body, sha(body))
	reg.realm = "http://" + strings.TrimPrefix(reg.srv.URL, "https://") + "/token"

	_, err := BundleDigests(reg.ref("darwin-arm64"), sha(body))
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("followed a token realm over plain http (err %v)", err)
	}
	if reg.tokenHit != 0 {
		t.Errorf("the plain-http realm was asked for a token")
	}
	var refused *ManifestRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("a plain-http realm is an answer brig refused, and the error does not say so: %T %v", err, err)
	}
}

// The https realm check means nothing if the realm can hand the request on.
// An https realm that redirects to plain http is refused at the redirect, and
// the plain-http host is never asked.
func TestBundleDigestsRefusesARealmThatRedirectsToPlainHTTP(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest})
	reg := newFakeRegistry(t, body, sha(body))
	plainHit := 0
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		plainHit++
		_, _ = w.Write([]byte(`{"token":"anon"}`))
	}))
	t.Cleanup(plain.Close)
	reg.realm = reg.srv.URL + "/redirect"
	reg.redirect = plain.URL + "/token"

	got, err := BundleDigests(reg.ref("darwin-arm64"), sha(body))
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("followed a redirect to plain http and read %v (err %v)", got, err)
	}
	if plainHit != 0 {
		t.Errorf("the plain-http redirect target was asked %d times", plainHit)
	}
	var refused *ManifestRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("a redirect to plain http is an answer brig refused, and the error does not say so: %T %v", err, err)
	}
}

// A realm that redirects to itself is a registry that answered, so the loop is
// a refusal and no provenance record stands in for it.
func TestBundleDigestsRefusesARedirectLoop(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest})
	reg := newFakeRegistry(t, body, sha(body))
	reg.realm = reg.srv.URL + "/redirect"
	reg.redirect = reg.srv.URL + "/redirect"

	got, err := BundleDigests(reg.ref("darwin-arm64"), sha(body))
	var refused *ManifestRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a redirect loop read %v and is not a refusal: %T %v", got, err, err)
	}
}

// A registry that does not answer, or answers the digest with nothing, is a
// different case from one that answered with bytes brig refused. Only this
// one lets hull's provenance record stand in, so it must not come back as a
// refusal.
func TestBundleDigestsThatGetNoAnswerAreNotARefusal(t *testing.T) {
	body := bundleManifest(ociManifest, map[string]string{"Image": kernelDigest})
	reg := newFakeRegistry(t, body, sha(body))

	_, err := BundleDigests(reg.ref("darwin-arm64"), sha([]byte("a digest the registry does not have")))
	var refused *ManifestRefusedError
	if err == nil || errors.As(err, &refused) {
		t.Errorf("a 404 came back as %T %v, want a plain error", err, err)
	}

	ref := reg.ref("darwin-arm64")
	reg.srv.Close()
	_, err = BundleDigests(ref, sha(body))
	if err == nil || errors.As(err, &refused) {
		t.Errorf("a registry that is down came back as %T %v, want a plain error", err, err)
	}
}

func writeProvenance(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "provenance.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	verifiedBundle = "sha256:e82a8e4da77499954ccdf78a8f013c4487d68a0a7139e35e7ca075796a33bc10"
	olderBundle    = "sha256:0e8f69d1ff8ad8ea0afeeaad0e426ddab050bc607fb8c4dc0b8fccb9db961c70"
)

func provenance(digest, verified string, files map[string]string) string {
	var entries []string
	for name, d := range files {
		entries = append(entries, fmt.Sprintf(`%q:{"sha256":%q,"size":1}`, name, strings.TrimPrefix(d, "sha256:")))
	}
	return fmt.Sprintf(`{"ref":"ghcr.io/nofireai/hull-assets:darwin-arm64","digest":%q,"verifiedDigest":%q,"files":{%s}}`,
		digest, verified, strings.Join(entries, ","))
}

// hull's record of a bundle other than the one that verified says the files
// on disk came from that other bundle. It is a different answer from "no
// record", so it comes back as its own error.
func TestProvenanceForAnotherDigestIsNotRead(t *testing.T) {
	files := map[string]string{"Image": kernelDigest, "container-initrd": initrdDigest}
	for _, tc := range []struct{ what, digest, verified string }{
		{"fetched and verified another bundle", olderBundle, olderBundle},
		{"fetched this bundle but verified another", verifiedBundle, olderBundle},
		{"fetched another bundle and names this one verified", olderBundle, verifiedBundle},
	} {
		dir := t.TempDir()
		writeProvenance(t, dir, provenance(tc.digest, tc.verified, files))

		got, err := ProvenanceDigests(dir, verifiedBundle)
		var other *OtherBundleError
		if !errors.As(err, &other) {
			t.Errorf("%s: got %v, %v, want an OtherBundleError", tc.what, got, err)
		}
	}
}

// The record that names the verified bundle gives its files' digests in the
// spelling BundleDigests uses.
func TestProvenanceForTheVerifiedDigestIsRead(t *testing.T) {
	dir := t.TempDir()
	writeProvenance(t, dir, provenance(verifiedBundle, verifiedBundle,
		map[string]string{"Image": kernelDigest, "container-initrd": initrdDigest}))

	got, err := ProvenanceDigests(dir, verifiedBundle)
	if err != nil {
		t.Fatalf("the record for the verified bundle was not read: %v", err)
	}
	if got["Image"] != kernelDigest || got["container-initrd"] != initrdDigest {
		t.Errorf("digests = %v", got)
	}
}

// No record, or one that is not JSON, is no answer at all.
func TestProvenanceThatIsMissingOrUnreadableIsAnError(t *testing.T) {
	if got, err := ProvenanceDigests(t.TempDir(), verifiedBundle); err == nil {
		t.Errorf("read %v from a directory with no record", got)
	}
	dir := t.TempDir()
	writeProvenance(t, dir, "not json")
	if got, err := ProvenanceDigests(dir, verifiedBundle); err == nil {
		t.Errorf("read %v from a record that is not JSON", got)
	}
}

func TestFileDigestIsTheSHA256OfTheBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Image")
	if err := os.WriteFile(path, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileDigest(path)
	if err != nil || got != kernelDigest {
		t.Errorf("FileDigest = %q, %v, want %q", got, err, kernelDigest)
	}
}
