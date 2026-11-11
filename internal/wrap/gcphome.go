package wrap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"github.com/brig-sh/brig/internal/profile"
)

// gcloudDir is the directory gcloud reads its logins from in the guest's
// home, and the directory of the application default credentials the Google
// client libraries read. A login there is a Google identity besides the gcp
// route's, which the run did not name: gcloud uses it whenever the guest has
// not pointed it at the route's token file, and a client library, which
// never reads that file, uses it to reach Google directly wherever egress
// allows. So on a run with a gcp route nothing the host hands the guest may
// put one there. A guest that gets one in some other way still reaches the
// listed services only through the broker, which takes the session token
// alone, and under deny egress no Google host directly.
const gcloudDir = ".config/gcloud"

// gcpShadowVars are the variables that hand gcloud or a Google client library
// an identity, pick one, or move where it reads one from: the identity
// variables the broker drops when it runs gcloud (gcpcred.droppedEnv),
// CLOUDSDK_CONFIG, which moves ~/.config/gcloud, and GCE_METADATA_*, which
// points a client at another metadata server. A name that ends in _ is a
// prefix. The preset's own variables, CLOUDSDK_API_ENDPOINT_OVERRIDES_* and
// CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE, are not on it.
//
// CLOUDSDK_AUTH_ACCESS_TOKEN_FILE is on it too, although the guest sets it to
// the route's token: from the profile it can as well name a token that a
// files: binding puts in the guest, which gcloud would use in place of the
// route while the run names the route's identity.
var gcpShadowVars = []string{
	"GOOGLE_APPLICATION_CREDENTIALS", "CLOUDSDK_CONFIG", "CLOUDSDK_CORE_ACCOUNT",
	"CLOUDSDK_ACTIVE_CONFIG_NAME", "CLOUDSDK_AUTH_", "GCE_METADATA_",
}

// gcpTokenFileVar is the variable the guest sets itself to the file it writes
// the route's session token to.
const gcpTokenFileVar = "CLOUDSDK_AUTH_ACCESS_TOKEN_FILE"

// gcpShadow reports whether name is on gcpShadowVars.
func gcpShadow(name string) bool {
	return slices.ContainsFunc(gcpShadowVars, func(v string) bool {
		if strings.HasSuffix(v, "_") {
			return strings.HasPrefix(name, v)
		}
		return name == v
	})
}

// gcpRoute reports whether this run has a gcp route.
func (c *Config) gcpRoute() bool {
	return slices.ContainsFunc(c.routes, isGCPRoute)
}

func isGCPRoute(r v1alpha1.Route) bool { return r.Preset == v1alpha1.PresetGCP }

// gcpList names each gcp route, with the policy that declares it.
func (c *Config) gcpList() string {
	var names []string
	for _, r := range c.routes {
		if isGCPRoute(r) {
			names = append(names, fmt.Sprintf("%s (route %s)", c.routePolicy[r.Name], r.Name))
		}
	}
	return strings.Join(names, ", ")
}

// gcpShadowRefusal refuses a run with a gcp route when the guest would also
// get a variable from gcpShadowVars, from the profile or BRIG_FORWARD_ENV. It
// refuses HOME too, unless the profile sets it to the guest home itself: HOME
// moves ~/.config/gcloud as CLOUDSDK_CONFIG does, and the checks of the
// profile and of the workspace look only at the guest home's.
func (c *Config) gcpShadowRefusal() error {
	if !c.gcpRoute() {
		return nil
	}
	var shadow []string
	for _, b := range c.Env {
		if (gcpShadow(b.Name) || c.movesHome(b)) && !slices.Contains(shadow, b.Name) {
			shadow = append(shadow, b.Name)
		}
	}
	if len(shadow) == 0 {
		return nil
	}
	msg := fmt.Sprintf("policy %s declares a gcp route, and %s also hands the guest %s, "+
		"which can give gcloud or a Google client library an identity besides the route's; remove it from the "+
		"profile and BRIG_FORWARD_ENV, or detach the policy", c.gcpList(), c.Profile.Name, strings.Join(shadow, ", "))
	if slices.Contains(shadow, gcpTokenFileVar) {
		msg += ". The guest sets " + gcpTokenFileVar + " itself, to the file it writes $BRIG_GCP_TOKEN to"
	}
	if slices.Contains(shadow, "HOME") {
		msg += ". HOME moves ~/" + gcloudDir + " away from the guest home " + c.Profile.GuestHome + ", which brig checks"
	}
	return errors.New(msg)
}

// movesHome reports whether b sets HOME to anything but the guest home. A
// value from a ref is not known before the run, so it counts as moving it.
func (c *Config) movesHome(b profile.EnvBinding) bool {
	if b.Name != "HOME" {
		return false
	}
	return b.Value == "" || len(b.RefList()) > 0 || slashClean(b.Value) != slashClean(c.Profile.GuestHome)
}

// onGCloudDir reports whether rel, relative to the guest home, is
// .config/gcloud, inside it, or .config, which holds it.
func onGCloudDir(rel string) bool {
	rel = slashClean(filepath.ToSlash(rel))
	return rel == gcloudDir || strings.HasPrefix(rel, gcloudDir+"/") || rel == filepath.Dir(gcloudDir)
}

// gcpHomeRefusal refuses a run with a gcp route whose profile would put
// something at ~/.config/gcloud in the guest, or at ~/.config, which holds
// it: a volume, a files: binding, a host config seed, or the onboarding file.
// A seed may not copy the host's own ~/.config/gcloud either, or a directory
// that holds it. A tmpfs at its own path is allowed: it carries nothing from
// the host, and over ~/.config/gcloud it keeps a login made in the guest off
// the home share. One mounted there with at: is not, since the hostmounts
// under its path are bound back in under it, from wherever that path is in
// the home. It reads the profile only; PrepareWorkspace checks the workspace
// itself.
func (c *Config) gcpHomeRefusal() error {
	if !c.gcpRoute() {
		return nil
	}
	var why []string
	home := slashClean(c.Profile.GuestHome)
	for _, v := range c.Profile.Volumes {
		if v.Kind != profile.VolumeTmpfs && v.Path != "" && onGCloudDir(v.Path) {
			why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.Path))
		}
		if v.At != "" {
			if rel, ok := strings.CutPrefix(slashClean(v.At), home+"/"); ok && onGCloudDir(rel) {
				why = append(why, fmt.Sprintf("a %s volume at %s", v.Kind, v.At))
			}
		}
	}
	for _, f := range c.Profile.Files {
		if onGCloudDir(f.Path) {
			why = append(why, fmt.Sprintf("the file %s from %s", f.Path, f.Ref))
		}
	}
	// seedOnboarding writes the seed after the workspace refusals have run,
	// so a seed there would boot on a fresh home.
	if ob := c.Profile.Onboarding; ob != nil && ob.File != "" && onGCloudDir(ob.File) {
		why = append(why, fmt.Sprintf("the onboarding file %s", ob.File))
	}
	hostGCloud := ""
	if h, err := os.UserHomeDir(); err == nil {
		hostGCloud = filepath.Join(h, filepath.FromSlash(gcloudDir))
	}
	for _, s := range c.HostConfig {
		switch {
		case onGCloudDir(s.Rel):
			why = append(why, fmt.Sprintf("a copy of %s at %s", s.Host, s.Rel))
		case hostGCloud != "" && overlaps(filepath.Clean(s.Host), hostGCloud):
			why = append(why, fmt.Sprintf("a copy of your %s", s.Host))
		}
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("policy %s declares a gcp route, and %s puts %s in the guest's ~/%s, "+
		"where gcloud and the Google client libraries read an identity besides the route's; remove it from "+
		"the profile, or detach the policy", c.gcpList(), c.Profile.Name, strings.Join(why, " and "), gcloudDir)
}

// overlaps reports whether one of the clean host paths a and b is the other
// or holds it.
func overlaps(a, b string) bool {
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// gcloudIdentityFiles are the files in ~/.config/gcloud that hold an
// identity: gcloud's stored logins and tokens, and the application default
// credentials. A configuration can point at one elsewhere; see
// gcloudConfigFiles. The rest is what gcloud in the guest writes there itself
// on its first run, such as active_config and logs/.
var gcloudIdentityFiles = []string{
	gcloudDir + "/credentials.db",
	gcloudDir + "/access_tokens.db",
	gcloudDir + "/application_default_credentials.json",
	gcloudDir + "/legacy_credentials",
}

// gcloudStores are the gcloud identity files that are SQLite databases.
// gcloud creates both, with their tables and no rows, whenever it opens its
// store, as `gcloud auth list` does with no account. Such a store holds no
// identity, so a kept home in which gcloud ran once is not refused for it.
var gcloudStores = []string{gcloudDir + "/credentials.db", gcloudDir + "/access_tokens.db"}

// maxStoreSize bounds the store read to decide whether it is empty. An empty
// store is three pages; anything larger than this is refused unread.
const maxStoreSize = 64 << 10

// gcloudConfigDir holds gcloud's named configurations, config_<name>, and
// gcloudLegacyProperties is the file gcloud turns into config_default when it
// finds no configuration. An [auth] property in either, such as
// credential_file_override or impersonate_service_account, hands gcloud an
// identity from elsewhere in the home, as credential_process does in
// .aws/config. The CLOUDSDK_AUTH_ variables that set the same properties are
// refused by gcpShadowRefusal.
const (
	gcloudConfigDir        = gcloudDir + "/configurations"
	gcloudLegacyProperties = gcloudDir + "/properties"
)

// maxGCloudConfigs bounds the configurations read; a directory with more is
// refused unread. maxConfigSize bounds each one: gcloud writes a few lines.
const (
	maxGCloudConfigs = 64
	maxConfigSize    = 16 << 10
)

// gcpWorkspaceRefusal refuses a run with a gcp route whose guest home already
// holds a gcloud identity file, such as a home named with --home ~. It looks
// through the workspace root: a link at .config/gcloud or at the file is not
// followed, and is refused like the file. A .config that leads out of the
// home is refused by the root. A .config or .config/gcloud that is not a
// directory holds nothing.
func (c *Config) gcpWorkspaceRefusal(r *workspaceRoot) error {
	if !c.gcpRoute() {
		return nil
	}
	info, err := r.lstat(gcloudDir)
	switch {
	case absent(err):
		return nil
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return c.gcloudFileRefusal(r, gcloudDir)
	}
	for _, rel := range gcloudIdentityFiles {
		info, err := r.lstat(rel)
		switch {
		case absent(err):
			continue
		case err != nil:
			return err
		}
		// Only a regular file is read. A link, even one that stays in the
		// home, is refused as before: its target can change after the read.
		if slices.Contains(gcloudStores, rel) && info.Mode().IsRegular() {
			empty, err := emptyStore(r, rel)
			if err != nil {
				return err
			}
			if empty {
				continue
			}
		}
		return c.gcloudFileRefusal(r, rel)
	}
	rels, err := gcloudConfigFiles(r)
	if err != nil {
		return err
	}
	for _, rel := range rels {
		if !authFreeConfig(r, rel) {
			return c.gcloudFileRefusal(r, rel)
		}
	}
	return nil
}

// gcloudConfigFiles lists the gcloud configurations under the workspace root
// whose properties gcloud reads: the legacy properties file and each
// configurations/config_<name>. A configurations that is a link is listed
// itself, so it is refused. More than maxGCloudConfigs entries are listed as
// the directory, refused unread.
func gcloudConfigFiles(r *workspaceRoot) ([]string, error) {
	rels := []string{gcloudLegacyProperties}
	info, err := r.lstat(gcloudConfigDir)
	switch {
	case absent(err):
		return rels, nil
	case err != nil:
		return nil, err
	case info.Mode()&os.ModeSymlink != 0:
		return append(rels, gcloudConfigDir), nil
	case !info.IsDir():
		return rels, nil
	}
	d, err := r.root.Open(gcloudConfigDir)
	if err != nil {
		return nil, r.refuse("read", gcloudConfigDir, err)
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(maxGCloudConfigs + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, r.refuse("read", gcloudConfigDir, err)
	}
	if len(entries) > maxGCloudConfigs {
		return append(rels, gcloudConfigDir), nil
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "config_") {
			rels = append(rels, gcloudConfigDir+"/"+e.Name())
		}
	}
	return rels, nil
}

// authFreeConfig reports whether the gcloud configuration at rel sets no
// [auth] property, or is not there. Only a regular file is read, through
// the root and bounded; anything else, a link or a file over maxConfigSize or
// that cannot be read, counts as setting one, so the check fails closed.
func authFreeConfig(r *workspaceRoot, rel string) bool {
	info, err := r.lstat(rel)
	switch {
	case absent(err):
		return true
	case err != nil, !info.Mode().IsRegular():
		return false
	}
	f, err := r.openRegular(rel, os.O_RDONLY)
	if err != nil {
		return absent(err)
	}
	defer func() { _ = f.Close() }()
	blob, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil || len(blob) > maxConfigSize {
		return false
	}
	return !setsAuth(string(blob))
}

// setsAuth reports whether the INI text of a gcloud configuration has a line
// that is not blank or a comment in an [auth] or a [DEFAULT] section: Python's
// configparser, which gcloud reads it with, falls back to DEFAULT for every
// section. The section name is trimmed and folded, so a spelling gcloud would
// not read is refused too. A lone CR ends a line, as it does for Python.
func setsAuth(text string) bool {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	auth := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "", strings.HasPrefix(t, "#"), strings.HasPrefix(t, ";"):
		case strings.HasPrefix(t, "["):
			name, _, _ := strings.Cut(strings.TrimPrefix(t, "["), "]")
			name = strings.TrimSpace(name)
			auth = strings.EqualFold(name, "auth") || strings.EqualFold(name, "default")
		case auth:
			return true
		}
	}
	return false
}

// absent reports whether err from a lookup in the workspace means nothing is
// there: the entry does not exist, or a directory on its path is a file.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// emptyStore reports whether the SQLite database at rel holds no row. It reads
// the file rather than linking SQLite; see emptySQLite. A rollback journal or
// write-ahead log beside the store can put rows back when gcloud opens it, so
// either one counts as a row. Anything it cannot read as empty, a file that is
// not a store or one over maxStoreSize, counts as a row too, so the check
// fails closed. The size is checked on the open file and the read is bounded,
// since the guest can grow the file after a stat.
func emptyStore(r *workspaceRoot, rel string) (bool, error) {
	for _, side := range []string{"-journal", "-wal"} {
		if _, err := r.lstat(rel + side); !absent(err) {
			return false, nil
		}
	}
	// The guest is live on the attach path and can remove or replace the
	// store after the lstat: gone counts as empty, anything else as a row.
	f, err := r.openRegular(rel, os.O_RDONLY)
	if err != nil {
		return absent(err), nil
	}
	defer func() { _ = f.Close() }()
	blob, err := io.ReadAll(io.LimitReader(f, maxStoreSize+1))
	if err != nil {
		return false, r.refuse("read", rel, err)
	}
	if len(blob) > maxStoreSize {
		return false, nil
	}
	return len(blob) == 0 || emptySQLite(blob), nil
}

// emptySQLite reports whether blob is a SQLite database that holds no row:
// its schema, on page 1, names only tables and indexes, each rooted at a page
// of the file, and every page after the first is an empty table or index
// leaf. A view or a trigger is refused, since either can answer a query with
// a row that no page holds. See https://www.sqlite.org/fileformat.html.
func emptySQLite(blob []byte) bool {
	const header = 100
	if len(blob) < header || string(blob[:16]) != "SQLite format 3\x00" {
		return false
	}
	size := int(binary.BigEndian.Uint16(blob[16:18]))
	if size == 1 {
		size = 1 << 16
	}
	if size < 512 || size&(size-1) != 0 || len(blob)%size != 0 {
		return false
	}
	pages := len(blob) / size
	// The count of free pages: a freed page may still hold a deleted row.
	if binary.BigEndian.Uint32(blob[36:40]) != 0 {
		return false
	}
	// UTF-8 text, the only encoding the schema check below reads.
	if binary.BigEndian.Uint32(blob[56:60]) != 1 {
		return false
	}
	const tableLeaf, indexLeaf = 0x0d, 0x0a
	for off := size; off < len(blob); off += size {
		kind, cells := blob[off], binary.BigEndian.Uint16(blob[off+3:off+5])
		if (kind != tableLeaf && kind != indexLeaf) || cells != 0 {
			return false
		}
	}
	return schemaOnlyTables(blob[:size], size-int(blob[20]), pages)
}

// schemaOnlyTables reports whether page 1, a table leaf of the schema, holds
// only tables and indexes rooted at pages 2 to pages. A schema that spills
// past page 1, or that this cannot parse, is refused.
func schemaOnlyTables(page []byte, usable, pages int) bool {
	const at = 100
	if page[at] != 0x0d {
		return false
	}
	cells := int(binary.BigEndian.Uint16(page[at+3 : at+5]))
	for i := range cells {
		p := at + 8 + 2*i
		if p+2 > len(page) {
			return false
		}
		cell := int(binary.BigEndian.Uint16(page[p : p+2]))
		if cell >= len(page) {
			return false
		}
		payload, n := sqliteVarint(page[cell:])
		if n == 0 || payload > uint64(usable-35) {
			return false // on an overflow page, which this does not follow
		}
		_, m := sqliteVarint(page[cell+n:])
		start := cell + n + m
		if m == 0 || start+int(payload) > len(page) {
			return false
		}
		cols, ok := sqliteRecord(page[start : start+int(payload)])
		if !ok || len(cols) < 5 {
			return false
		}
		// type, name, tbl_name, rootpage, sql
		kind, root := string(cols[0].text), cols[3].int
		if (kind != "table" && kind != "index") || !cols[3].isInt || root < 2 || root > int64(pages) {
			return false
		}
		if !schemaSQL(kind, string(cols[1].text), cols[4]) {
			return false
		}
	}
	return true
}

// schemaSQL reports whether sql, the statement SQLite builds a schema entry
// from, creates the kind the entry claims. SQLite reads the statement, not
// the type column, so an entry typed table whose statement creates a view is
// a view. Only SQLite's own index for a key has no statement. Anything else,
// a comment or VIRTUAL included, is refused.
func schemaSQL(kind, name string, sql sqliteValue) bool {
	if sql.text == nil {
		return kind == "index" && !sql.isInt && strings.HasPrefix(name, "sqlite_autoindex_")
	}
	words := strings.Fields(strings.ToUpper(string(sql.text)))
	if len(words) < 3 || words[0] != "CREATE" {
		return false
	}
	switch kind {
	case "table":
		return words[1] == "TABLE"
	default:
		return words[1] == "INDEX" || (words[1] == "UNIQUE" && words[2] == "INDEX")
	}
}

// sqliteVarint decodes a SQLite varint and returns it with its length, or a
// length of 0 when b ends inside it.
func sqliteVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return v<<8 | uint64(b[i]), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// sqliteValue is one column of a record: an integer or the bytes of a text
// or blob.
type sqliteValue struct {
	isInt bool
	int   int64
	text  []byte
}

// sqliteRecord decodes a record, the payload of a table row.
func sqliteRecord(b []byte) ([]sqliteValue, bool) {
	hlen, n := sqliteVarint(b)
	if n == 0 || hlen > uint64(len(b)) {
		return nil, false
	}
	var cols []sqliteValue
	body := int(hlen)
	for h := n; h < int(hlen); {
		t, m := sqliteVarint(b[h:int(hlen)])
		if m == 0 {
			return nil, false
		}
		h += m
		var n64 uint64
		v := sqliteValue{}
		switch {
		case t == 0:
		case t >= 1 && t <= 6:
			n64 = []uint64{1, 2, 3, 4, 6, 8}[t-1]
			v.isInt = true
		case t == 7:
			n64 = 8
		case t == 8 || t == 9:
			v.isInt, v.int = true, int64(t-8)
		case t >= 12:
			n64 = (t - 12) / 2
		default:
			return nil, false
		}
		// Compared before it becomes an int: a serial type near 2^64 would
		// otherwise overflow body+size and pass the bound.
		if n64 > uint64(len(b)-body) {
			return nil, false
		}
		size := int(n64)
		if v.isInt && size > 0 {
			x := int64(int8(b[body])) // big-endian, two's complement
			for _, c := range b[body+1 : body+size] {
				x = x<<8 | int64(c)
			}
			v.int = x
		} else if t >= 12 {
			v.text = b[body : body+size]
		}
		body += size
		cols = append(cols, v)
	}
	return cols, true
}

func (c *Config) gcloudFileRefusal(r *workspaceRoot, rel string) error {
	return fmt.Errorf("policy %s declares a gcp route, and the guest home %s holds %s, "+
		"an identity besides the route's that gcloud and the Google client libraries read; move it out of "+
		"the guest home, or run with another --home", c.gcpList(), r.dir, rel)
}
