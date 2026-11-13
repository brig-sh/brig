// Package brigfile reads a Brigfile: the file a repository keeps at its root
// to say how it is run under brig. It names profiles and policies by alias,
// and targets that pair one profile with policies, a size and default
// arguments.
//
// A Brigfile comes from whoever wrote the repository, not from the person
// running brig, so this package reads it as untrusted input. It reads only
// files under the project root, through an os.Root. It refuses a repository
// profile that names anything on the host: a runtime binary, a host
// directory, or a place to import a credential from. It runs nothing, pulls
// nothing, reads no secret and writes nothing. Deciding whether to run what
// it describes is for the caller.
//
// Load reads every file a Brigfile names once and keeps the bytes, so a
// caller can digest and run from that one snapshot without reading the
// repository a second time.
package brigfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
	"sigs.k8s.io/yaml"
)

// FileName is the name of the file Discover looks for.
const FileName = "Brigfile"

// APIVersion is the only apiVersion a Brigfile may declare. It is the
// version of the policy documents too, so one repository reads one version.
const APIVersion = "brig.sh/v1alpha1"

// MaxFileSize bounds the Brigfile and every file it names. A repository
// chooses these files, and the YAML decoder holds a whole document in memory
// several times over, so an unbounded read is the repository's to size.
const MaxFileSize = 1 << 20

// builtinScheme is the only scheme a profile alias's from: takes.
const builtinScheme = "builtin:"

// Project is a Brigfile as written.
type Project struct {
	// APIVersion is required and must be APIVersion.
	APIVersion string `json:"apiVersion"`
	// Name is a label for display. Nothing is keyed on it.
	Name string `json:"name,omitempty"`
	// DefaultTarget names the target a run uses when it names none. Optional.
	DefaultTarget string `json:"defaultTarget,omitempty"`
	// Profiles maps an alias to where its profile comes from.
	Profiles map[string]ProfileAlias `json:"profiles,omitempty"`
	// Policies maps an alias to where its policy comes from.
	Policies map[string]PolicyAlias `json:"policies,omitempty"`
	// Targets maps a target name to what it runs. At least one is required.
	Targets map[string]Target `json:"targets"`
}

// ProfileAlias says where one profile comes from. Exactly one field is set.
type ProfileAlias struct {
	// From is builtin:<name>, a profile brig ships. No other scheme is read.
	From string `json:"from,omitempty"`
	// File is a profile document at a path relative to the project root.
	File string `json:"file,omitempty"`
	// Inline is a complete profile document, held as JSON.
	Inline json.RawMessage `json:"inline,omitempty"`
}

// PolicyAlias says where one policy comes from. Exactly one field is set.
type PolicyAlias struct {
	// File is a policy document at a path relative to the project root.
	File string `json:"file,omitempty"`
	// Inline is a complete policy document, held as JSON.
	Inline json.RawMessage `json:"inline,omitempty"`
}

// Target is one way to run the repository.
type Target struct {
	// Profile is a profile alias. Required.
	Profile string `json:"profile"`
	// Policies are policy aliases, each at most once.
	Policies []string `json:"policies,omitempty"`
	// Overrides changes the size or image of the profile for this target.
	Overrides *Overrides `json:"overrides,omitempty"`
	// Args are the arguments the guest command gets when the run gives none.
	Args []string `json:"args,omitempty"`
}

// Overrides is what a target may change about its profile. Each field is a
// pointer so that a value written as 0 or "" is refused, not read as unset.
type Overrides struct {
	// CPUs is the vCPU count.
	CPUs *int `json:"cpus,omitempty"`
	// Mem is the guest memory in MB.
	Mem *int `json:"mem,omitempty"`
	// Image is the guest image.
	Image *string `json:"image,omitempty"`
}

// Parse decodes a Brigfile and checks it on its own: its fields, its names,
// its file references, and that every alias a target names is declared. It
// does no I/O. The profile and policy documents the Brigfile names, inline
// ones included, are parsed by Load.
//
// The decode is strict. A key given twice, a key in the wrong case, an
// unknown key, a second document, and a scalar that YAML reads as a number
// or a boolean where a string belongs are all refused, with the path of the
// key in the error.
func Parse(blob []byte) (Project, error) {
	if len(blob) > MaxFileSize {
		return Project{}, fmt.Errorf("the Brigfile is %d bytes, more than the %d MiB limit",
			len(blob), MaxFileSize>>20)
	}
	// Strict into a plain tree first. This is the decode that refuses a key
	// given twice, which is how a duplicate alias or target is caught, and the
	// one that hits the YAML library's bound on alias expansion.
	var tree any
	if err := yaml.UnmarshalStrict(blob, &tree); err != nil {
		return Project{}, err
	}
	if err := checkTree(tree, reflect.TypeOf(Project{}), ""); err != nil {
		return Project{}, err
	}
	// v1alpha1.UnmarshalStrict is the rule every policy document is held to:
	// one document, exact keys, and no block or list left with no value.
	var p Project
	if err := v1alpha1.UnmarshalStrict(blob, &p); err != nil {
		return Project{}, err
	}
	return p, p.validate()
}

func (p Project) validate() error {
	switch {
	case p.APIVersion == "":
		return fmt.Errorf("apiVersion is required; this brig reads %s", APIVersion)
	case p.APIVersion != APIVersion:
		return fmt.Errorf("apiVersion %q is not supported; this brig reads %s", p.APIVersion, APIVersion)
	}
	// The name is printed, so a control character in it would reach the
	// terminal of whoever lists the targets.
	if strings.IndexFunc(p.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("name %q holds a control character", p.Name)
	}
	for _, alias := range sortedKeys(p.Profiles) {
		if err := checkName("profiles", alias); err != nil {
			return err
		}
		if err := p.Profiles[alias].validate("profiles." + alias); err != nil {
			return err
		}
	}
	for _, alias := range sortedKeys(p.Policies) {
		if err := checkName("policies", alias); err != nil {
			return err
		}
		if err := p.Policies[alias].validate("policies." + alias); err != nil {
			return err
		}
	}
	if len(p.Targets) == 0 {
		return errors.New("targets: at least one target is required")
	}
	for _, name := range sortedKeys(p.Targets) {
		if err := checkName("targets", name); err != nil {
			return err
		}
		if err := p.validateTarget(name, p.Targets[name]); err != nil {
			return err
		}
	}
	if p.DefaultTarget != "" {
		if _, ok := p.Targets[p.DefaultTarget]; !ok {
			return fmt.Errorf("defaultTarget %q names no target", p.DefaultTarget)
		}
	}
	return nil
}

// checkName holds an alias or target name to the rule for a policy name. The
// names reach the command line and, later, session names, so they have to be
// safe as bare words. The YAML check also catches a key such as on:, which
// YAML 1.1 reads as the boolean true, so that the alias would be named
// "true" while the file says on.
func checkName(section, name string) error {
	if err := v1alpha1.CheckName(name); err != nil {
		return fmt.Errorf("%s: %w", section, err)
	}
	return nil
}

func (a ProfileAlias) validate(where string) error {
	var set []string
	if a.From != "" {
		set = append(set, "from")
	}
	if a.File != "" {
		set = append(set, "file")
	}
	if len(a.Inline) > 0 {
		set = append(set, "inline")
	}
	if len(set) != 1 {
		return forms(where, set, "from, file or inline")
	}
	switch {
	case a.From != "":
		name, ok := strings.CutPrefix(a.From, builtinScheme)
		if !ok || name == "" {
			return fmt.Errorf("%s.from: %q is not builtin:<name>; a Brigfile names a profile brig "+
				"ships as builtin:<name>, and any other profile with file: or inline:", where, a.From)
		}
		if err := v1alpha1.CheckName(name); err != nil {
			return fmt.Errorf("%s.from: %w", where, err)
		}
	case a.File != "":
		if err := checkRef(a.File); err != nil {
			return fmt.Errorf("%s.file: %w", where, err)
		}
	default:
		if err := isMapping(a.Inline); err != nil {
			return fmt.Errorf("%s.inline: %w", where, err)
		}
	}
	return nil
}

func (a PolicyAlias) validate(where string) error {
	var set []string
	if a.File != "" {
		set = append(set, "file")
	}
	if len(a.Inline) > 0 {
		set = append(set, "inline")
	}
	if len(set) != 1 {
		return forms(where, set, "file or inline")
	}
	if a.File != "" {
		if err := checkRef(a.File); err != nil {
			return fmt.Errorf("%s.file: %w", where, err)
		}
		return nil
	}
	if err := isMapping(a.Inline); err != nil {
		return fmt.Errorf("%s.inline: %w", where, err)
	}
	return nil
}

// forms is the refusal for an alias that sets no declaration form or more
// than one. Two forms have no safe precedence: either answer surprises
// whoever wrote both.
func forms(where string, set []string, choices string) error {
	if len(set) == 0 {
		return fmt.Errorf("%s: set one of %s", where, choices)
	}
	return fmt.Errorf("%s: sets %s; set exactly one of %s", where, strings.Join(set, " and "), choices)
}

// isMapping refuses an inline value that is not a mapping. The decoder holds
// inline: as raw JSON, so a scalar or a list would otherwise reach the
// profile or policy parser and fail there with an error about a Go type.
func isMapping(raw json.RawMessage) error {
	if s := strings.TrimSpace(string(raw)); !strings.HasPrefix(s, "{") {
		return errors.New("must be a mapping that holds the whole document")
	}
	return nil
}

func (p Project) validateTarget(name string, t Target) error {
	where := "targets." + name
	if t.Profile == "" {
		return fmt.Errorf("%s.profile is required", where)
	}
	if _, ok := p.Profiles[t.Profile]; !ok {
		return fmt.Errorf("%s.profile: %q is not an alias under profiles", where, t.Profile)
	}
	seen := map[string]bool{}
	for _, alias := range t.Policies {
		if _, ok := p.Policies[alias]; !ok {
			return fmt.Errorf("%s.policies: %q is not an alias under policies", where, alias)
		}
		if seen[alias] {
			return fmt.Errorf("%s.policies: %q is listed twice", where, alias)
		}
		seen[alias] = true
	}
	if o := t.Overrides; o != nil {
		if o.CPUs != nil && *o.CPUs <= 0 {
			return fmt.Errorf("%s.overrides.cpus: %d is not greater than zero", where, *o.CPUs)
		}
		if o.Mem != nil && *o.Mem <= 0 {
			return fmt.Errorf("%s.overrides.mem: %d is not greater than zero", where, *o.Mem)
		}
		if o.Image != nil && (*o.Image == "" || strings.IndexFunc(*o.Image, unicode.IsSpace) >= 0 ||
			strings.IndexFunc(*o.Image, unicode.IsControl) >= 0) {
			return fmt.Errorf("%s.overrides.image: %q is not an image reference", where, *o.Image)
		}
	}
	return nil
}

// checkRef refuses a file reference that is not a plain path below the
// project root. os.Root would refuse one that leaves the root anyway; this
// says so before any I/O, and it also refuses a second spelling of a path
// that stays inside, such as ./a or a//b, so that one file has one key in
// Loaded.Files.
func checkRef(ref string) error {
	switch {
	case ref == "":
		return errors.New("is empty")
	case strings.HasPrefix(ref, "/"):
		return fmt.Errorf("%q is absolute; a path is relative to the directory that holds the Brigfile", ref)
	case strings.Contains(ref, `\`):
		return fmt.Errorf("%q holds a backslash; separate a path with /", ref)
	case strings.HasPrefix(ref, "~"):
		return fmt.Errorf("%q starts with ~; a path is relative to the directory that holds the Brigfile", ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if part == ".." {
			return fmt.Errorf("%q has a .. component; a Brigfile names only files below its own directory", ref)
		}
	}
	if ref == "." || !fs.ValidPath(ref) {
		return fmt.Errorf("%q is not a plain path; write it with no ., no empty component and no trailing /", ref)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
