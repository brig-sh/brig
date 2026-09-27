package verify

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// timeoutDetail is Detail for a cosign cut off at cosignTimeout. It is built
// here and not from cosign's output: what cosign printed before it was killed
// is its deprecation warning, or nothing, and neither is the reason.
func timeoutDetail(ref string) string {
	detail := fmt.Sprintf("cosign did not answer within %s", cosignTimeout)
	if path, helper, key := credentialHelper(ref); helper != "" {
		// cosign inherits brig's environment, and brigd's when brigd runs the
		// boot, so DOCKER_CONFIG has to be set where brigd starts too.
		detail += fmt.Sprintf(". It waits on docker-credential-%s, set by %s in %s. "+
			"Start the app that helper belongs to, or run brig with DOCKER_CONFIG set "+
			"to an empty directory (and restart brigd with it set, if brigd is running)",
			helper, key, path)
	}
	return detail
}

// dockerConfig is the part of Docker's config.json that names credential
// helpers. The file also holds registry credentials under "auths", and those
// are not decoded, so nothing from them can reach a message.
// internal/wrap/workspace.go treats the same file as sensitive for the same
// reason.
type dockerConfig struct {
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// credentialHelper names the Docker credential helper cosign asks for ref's
// registry, where it is set, and under which key. It returns empty strings
// when no helper is set or the file cannot be read or parsed.
//
// cosign reads $DOCKER_CONFIG/config.json, or ~/.docker/config.json when
// DOCKER_CONFIG is unset or empty. A credHelpers entry for the host wins over
// credsStore, the same order Docker's keychain uses.
//
// It is read only after cosign has timed out, so a normal boot does not open
// the file.
func credentialHelper(ref string) (path, helper, key string) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", ""
		}
		dir = filepath.Join(home, ".docker")
	}
	path = filepath.Join(dir, "config.json")
	b, err := readSmallFile(path)
	if err != nil {
		return "", "", ""
	}
	var cfg dockerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", ""
	}
	// credHelpers is keyed on the bare host, and normalizeRef gives the host
	// in the spelling Docker uses: lower case, no default port.
	host, _, _ := strings.Cut(normalizeRef(ref), "/")
	if h := cfg.CredHelpers[host]; h != "" {
		return path, h, "credHelpers"
	}
	if cfg.CredsStore != "" {
		return path, cfg.CredsStore, "credsStore"
	}
	return "", "", ""
}

// maxDockerConfig caps the read. A real config.json is a few kilobytes; one
// larger than this is not parsed, and the hint names no helper.
const maxDockerConfig = 1 << 20

// readSmallFile reads path only when it is a regular file, and at most
// maxDockerConfig bytes of it. This runs after cosign has already been cut off,
// and a FIFO or a device in place of config.json would otherwise block the
// read and turn a bounded timeout into a hang with no message at all.
func readSmallFile(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxDockerConfig))
}
