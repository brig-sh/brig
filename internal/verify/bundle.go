package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The boot bundle's signature covers its manifest, and the manifest lists
// each file with its own sha256. So the digest that verified pins the bytes
// of the kernel and the initrd, provided brig reads the manifest by that
// digest and checks the files against it. This file is that reading (#234).

// BootDigests are the sha256 digests of the files in one boot bundle, by file
// name, each spelled "sha256:<hex>".
type BootDigests map[string]string

// OtherBundleError is a provenance record that names a bundle other than the
// one that verified. The files beside it came from that other bundle, which
// is a different answer from "there is no record", so it has a type of its
// own.
type OtherBundleError struct {
	Dir      string
	Recorded string
	Verified string
}

func (e *OtherBundleError) Error() string {
	return fmt.Sprintf("hull's record in %s says these files came from %s, not the %s that verified",
		e.Dir, e.Recorded, e.Verified)
}

// ManifestRefusedError is a registry that answered the manifest request with
// something brig refuses: bytes that are not the digest it asked for, an
// index, a body too large to be a bundle manifest, or a token realm or
// redirect over plain http. hull's provenance record stands in for a registry
// that did not answer. A registry that answered wrongly is a different case,
// and falling back on the record there forgets that it did, so it has a type
// of its own.
type ManifestRefusedError struct{ Reason string }

func (e *ManifestRefusedError) Error() string { return e.Reason }

func refusedf(format string, a ...any) error {
	return &ManifestRefusedError{Reason: fmt.Sprintf(format, a...)}
}

// registryClient makes the manifest requests. A variable so a test can point
// it at a TLS server of its own. It keeps http.DefaultTransport, and with it
// the proxy settings from the environment. registryDo adds the redirect
// policy on every request, so a client a test swaps in gets it too.
var registryClient = http.DefaultClient

// registryDo sends req with registryClient, following only https redirects.
// The https check on the token realm holds only if the realm cannot then
// hand the request on to plain http, and the same goes for the manifest.
func registryDo(req *http.Request) (*http.Response, error) {
	c := *registryClient
	c.CheckRedirect = httpsRedirectsOnly
	return c.Do(req)
}

func httpsRedirectsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return refusedf("the registry redirected to %s, and brig follows only https", req.URL.Redacted())
	}
	// The limit http.Client applies when CheckRedirect is nil. A registry
	// that redirects past it has answered, so no provenance record stands in.
	if len(via) >= 10 {
		return refusedf("the registry redirected more than 10 times")
	}
	return nil
}

// registryTimeout bounds the manifest fetch, token request included, for the
// reason cosignTimeout bounds cosign: a registry that never answers must not
// hold the boot.
var registryTimeout = 30 * time.Second

// maxManifest is far above the few hundred bytes a bundle manifest takes. A
// registry that sends more is not sending one.
const maxManifest = 1 << 20

const (
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	annotationTitle     = "org.opencontainers.image.title"
)

// BundleDigests reads the per-file digests from the manifest of ref at
// digest, the one whose signature verified.
//
// Fetched by digest, never by tag, and the bytes are hashed against that
// digest before anything in them is read. A tag can move between the
// signature check and this request. A digest names one manifest, and a
// registry that answers it with other bytes is refused here.
//
// Standard library only, with the anonymous token flow the public ghcr
// packages take. brig shells out to cosign and oras for everything else.
// `oras manifest fetch` prints the same manifest, but this runs on every
// verified boot, where oras is otherwise needed only to download a missing
// bundle. In process the request carries the 30 second and 1 MiB bounds
// below, refuses a plain-http realm or redirect, and hashes the exact bytes
// the registry sent, with no binary to find and no output to parse.
func BundleDigests(ref, digest string) (BootDigests, error) {
	host, repo, err := splitRef(ref)
	if err != nil {
		return nil, err
	}
	if !isDigest(digest) {
		return nil, fmt.Errorf("%q is not a sha256 digest", digest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), registryTimeout)
	defer cancel()

	u := "https://" + host + "/v2/" + repo + "/manifests/" + digest
	body, err := getManifest(ctx, u, "")
	var challenge *authChallenge
	if errors.As(err, &challenge) {
		token, terr := anonymousToken(ctx, challenge.header)
		if terr != nil {
			return nil, terr
		}
		body, err = getManifest(ctx, u, token)
	}
	if err != nil {
		return nil, err
	}

	if got := sha256Digest(body); got != digest {
		return nil, refusedf("the registry answered %s with a manifest whose digest is %s", digest, got)
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Layers    []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, refusedf("the manifest at %s is not JSON: %v", digest, err)
	}
	if m.MediaType != mediaOCIManifest && m.MediaType != mediaDockerManifest {
		return nil, refusedf("the manifest at %s is a %q, not an image manifest", digest, m.MediaType)
	}
	files := BootDigests{}
	for _, l := range m.Layers {
		name := l.Annotations[annotationTitle]
		if name == "" || !isDigest(l.Digest) {
			continue
		}
		files[name] = l.Digest
	}
	return files, nil
}

// authChallenge is a 401 carrying a WWW-Authenticate header, the registry's
// way of asking for a token first.
type authChallenge struct{ header string }

func (a *authChallenge) Error() string { return "the registry asked for a token: " + a.header }

func getManifest(ctx context.Context, u, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", mediaOCIManifest+", "+mediaDockerManifest)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := registryDo(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized && token == "" {
		if h := resp.Header.Get("WWW-Authenticate"); h != "" {
			return nil, &authChallenge{header: h}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxManifest {
		return nil, refusedf("GET %s: the manifest is larger than %d bytes", u, maxManifest)
	}
	return body, nil
}

// anonymousToken asks the realm a Bearer challenge names for a pull token,
// with no credentials.
//
// The realm is whatever the registry's answer says, so only an https one is
// followed. A plain-http realm carries the request in the clear to a host
// the answer chose.
func anonymousToken(ctx context.Context, challenge string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("the registry asked for %s authentication, and brig reads the bundle anonymously", scheme)
	}
	p := parseChallenge(params)
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", refusedf("the registry named token realm %q, and brig follows only an https one", p["realm"])
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if v := p[k]; v != "" {
			q.Set(k, v)
		}
	}
	realm.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := registryDo(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request to %s: %s", realm.Host, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifest)).Decode(&t); err != nil {
		return "", fmt.Errorf("token request to %s: %w", realm.Host, err)
	}
	if t.Token != "" {
		return t.Token, nil
	}
	if t.AccessToken != "" {
		return t.AccessToken, nil
	}
	return "", fmt.Errorf("token request to %s returned no token", realm.Host)
}

// parseChallenge reads the key="value" pairs of a WWW-Authenticate header. A
// scope carries commas inside its quotes, so the split respects them.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, " ,")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				val, s = rest[1:], ""
			} else {
				val, s = rest[1:1+end], rest[2+end:]
			}
		} else {
			val, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(strings.TrimSpace(key))] = val
	}
	return out
}

// splitRef takes a reference apart into the registry host and the repository
// path, dropping a tag or a digest.
func splitRef(ref string) (host, repo string, err error) {
	s := normalizeRef(ref)
	s = strings.TrimSuffix(refWithDigest(s, ""), "@")
	host, repo, ok := strings.Cut(s, "/")
	if !ok || host == "" || repo == "" || !strings.ContainsAny(host, ".:") {
		return "", "", fmt.Errorf("%q names no registry host", ref)
	}
	return host, repo, nil
}

// ProvenanceDigests reads the per-file digests from the provenance.json hull
// writes beside the assets it fetched.
//
// The fallback for a registry brig cannot reach, and only for a record of the
// bundle that verified. hull records the digest it fetched and the one whose
// signature it checked. If either names another bundle, the files beside it
// came from that bundle, and that comes back as an OtherBundleError.
func ProvenanceDigests(dir, digest string) (BootDigests, error) {
	path := filepath.Join(dir, "provenance.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec struct {
		Digest         string `json:"digest"`
		VerifiedDigest string `json:"verifiedDigest"`
		Files          map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("%s is not JSON: %w", path, err)
	}
	if rec.Digest == "" && rec.VerifiedDigest == "" {
		return nil, fmt.Errorf("%s names no bundle digest", path)
	}
	for _, recorded := range []string{rec.Digest, rec.VerifiedDigest} {
		if recorded != "" && recorded != digest {
			return nil, &OtherBundleError{Dir: dir, Recorded: recorded, Verified: digest}
		}
	}
	files := BootDigests{}
	for name, f := range rec.Files {
		if d := "sha256:" + f.SHA256; isDigest(d) {
			files[name] = d
		}
	}
	return files, nil
}

// FileDigest is the sha256 of the file at path, spelled as BootDigests
// spells it.
func FileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func isDigest(s string) bool {
	hexPart, ok := strings.CutPrefix(s, "sha256:")
	return ok && len(hexPart) == 64 && isHex(hexPart)
}
