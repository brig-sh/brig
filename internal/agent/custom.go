package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Custom templates.
//
// A template is data, so there is no reason the set of them has to be the one
// compiled in. Any Linux CLI in an OCI image already runs under brig; a
// template just saves you spelling out the image, the home directory and the
// credential variables every time.
//
// They live as one file per template in a directory, which makes them
// diffable, reviewable and easy to share -- and means `brig export` followed
// by an edit is the way to write one, rather than starting from a blank file
// and a guess about the field names.
//
// YAML or JSON, read the same way: JSON is a subset of YAML, so one parser
// handles both and nothing has to guess at a format. Export writes YAML,
// because a template is a file a person edits and YAML has comments; JSON
// stays available for anything generating templates programmatically. An
// imported file is stored byte for byte as you wrote it, so comments and
// ordering survive the round trip.
//
// See https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md
// for how to build an image for one.

// BringYourOwnImageDoc is where to send someone who wants their own image.
const BringYourOwnImageDoc = "https://github.com/brig-sh/community-images/blob/main/docs/bring-your-own-image.md"

// TemplateDir is where custom templates live. BRIG_TEMPLATE_DIR overrides it.
func TemplateDir() string {
	if dir := os.Getenv("BRIG_TEMPLATE_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".brig/templates"
	}
	return filepath.Join(home, ".config", "brig", "templates")
}

// custom holds the templates loaded from disk, by name.
var custom = map[string]Template{}

// LoadCustom reads every template in a directory and registers it.
//
// A custom template with a built-in's name replaces it, deliberately: that is
// how you pin your own image or your own forwarded variables for an agent
// brig already knows about, without having to invent a second name for it.
//
// A directory that does not exist is not an error -- most installs have none.
func LoadCustom(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var problems []string
	for _, e := range entries {
		if e.IsDir() || !isTemplateFile(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		t, err := ReadTemplate(path)
		if err != nil {
			// One bad file must not take the others down with it: report it
			// and carry on, so a typo in a template you are not using does
			// not stop the agent you are.
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		custom[t.Name] = t
	}
	if len(problems) > 0 {
		return fmt.Errorf("ignoring %d unusable template(s):\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

// isTemplateFile reports whether a directory entry is a template. Anything
// else in the directory -- a README, an editor's backup -- is ignored rather
// than reported as broken.
func isTemplateFile(name string) bool {
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// ReadTemplate reads and validates one template file.
func ReadTemplate(path string) (Template, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return Template{}, err
	}
	return ParseTemplate(blob)
}

// ParseTemplate decodes and validates a template, in YAML or JSON.
//
// One path serves both because JSON is a subset of YAML. The strict decoder
// refuses unknown fields rather than ignoring them: a misspelled "forwards"
// would otherwise decode into nothing and silently forward no credentials,
// which looks exactly like a broken sandbox.
func ParseTemplate(blob []byte) (Template, error) {
	var t Template
	if err := yaml.UnmarshalStrict(blob, &t); err != nil {
		return Template{}, err
	}
	return t, t.Validate()
}

// Validate checks a template is usable and safe.
func (t Template) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	// The name reaches a workspace path and a sandbox name, so it has to be
	// safe in both. Same rule as a session name, for the same reason.
	if !safeName(t.Name) {
		return fmt.Errorf("name %q may use only lowercase letters, digits, dot, "+
			"dash and underscore, and must start with a letter or digit", t.Name)
	}
	if t.Image == "" {
		return fmt.Errorf("image is required (see %s)", BringYourOwnImageDoc)
	}
	if t.GuestHome == "" || !strings.HasPrefix(t.GuestHome, "/") {
		return fmt.Errorf("guestHome is required and must be absolute, e.g. /home/%s", t.Name)
	}
	if t.Binary == "" && !t.GUI && !t.Shell {
		return fmt.Errorf("binary is required unless the template is gui or shell")
	}
	for _, d := range t.Deny {
		for _, f := range t.Forward {
			if d == f {
				return fmt.Errorf("%s is in both forward and deny", d)
			}
		}
	}
	if t.Mem <= 0 || t.CPUs <= 0 {
		return fmt.Errorf("mem and cpus must be greater than zero")
	}
	return nil
}

func safeName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '.' || r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Import validates a template and writes it into the template directory,
// where the next brig invocation will find it. It returns the path written.
//
// The bytes are stored exactly as they came in, rather than re-serialised
// from the parsed struct. A template is a file someone wrote, and
// round-tripping it through a marshaller would drop their comments and
// reorder their fields for no gain -- brig has already checked it parses.
func Import(blob []byte, dir string) (Template, string, error) {
	t, err := ParseTemplate(blob)
	if err != nil {
		return Template{}, "", err
	}
	if owner, reserved := Reserved(t.Name); reserved && t.Name != owner {
		return Template{}, "", fmt.Errorf("name %q collides with the %s template", t.Name, owner)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Template{}, "", err
	}
	path := filepath.Join(dir, t.Name+extensionFor(blob))
	// A template of this name may already be here in the other format, and
	// leaving both would make which one wins a matter of directory order.
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		other := filepath.Join(dir, t.Name+ext)
		if other != path {
			_ = os.Remove(other)
		}
	}
	if len(blob) > 0 && blob[len(blob)-1] != '\n' {
		blob = append(blob, '\n')
	}
	return t, path, os.WriteFile(path, blob, 0o644)
}

// extensionFor names the file after the format it is actually in, so what is
// on disk reads the way it was written.
func extensionFor(blob []byte) string {
	if len(strings.TrimSpace(string(blob))) > 0 && strings.TrimSpace(string(blob))[0] == '{' {
		return ".json"
	}
	return ".yaml"
}

// exportHeader is what makes an exported template a starting point rather
// than a puzzle. It is the one thing JSON could not carry, and the reason
// export writes YAML: the fields are explained where you are editing them.
const exportHeader = `# A brig agent template. Edit it, then: brig import <this file>
#
#   name       the template name. Also the workspace directory and the sandbox
#              name, so: lowercase letters, digits, dot, dash, underscore
#   image      the guest image to boot. brig can only verify the signature of
#              an image published under ghcr.io/brig-sh; anything else boots
#              with a warning
#   guestHome  where the workspace is mounted. The agent's state lands here,
#              which is what makes the workspace the unit of persistence
#   binary     the agent CLI inside the guest. Omit it for shell: true
#   forward    variables carried in from your environment when set. brig
#              resolves nothing itself, so any secret backend works
#   deny       variables never forwarded, whatever forward says. This is the
#              billing guard: a metered API key that outranks a subscription
#              token belongs here
#   statePaths paths under guestHome holding the agent's state, for reference
#   shell      the "agent" is the guest shell itself
#   gui        the agent opens a window; there is nothing to pass arguments to
#
# Building an image for a template:
# ` + BringYourOwnImageDoc + `
`

// Export renders a template as YAML, which is the starting point for writing
// one: export the closest agent, change the image and the name, import it
// back. YAML because the result is meant to be edited, and a template you
// cannot annotate is a template nobody will explain to the next person.
//
// Fields come out in alphabetical order rather than the order they are
// declared in, which is what round-tripping through JSON costs. The header
// carries the meaning, so the ordering matters less than being able to say
// what a field is for.
func Export(t Template) ([]byte, error) {
	body, err := yaml.Marshal(t)
	if err != nil {
		return nil, err
	}
	return append([]byte(exportHeader), body...), nil
}

// ExportJSON renders a template as JSON, for anything generating or consuming
// templates programmatically.
func ExportJSON(t Template) ([]byte, error) {
	blob, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(blob, '\n'), nil
}

// IsCustom reports whether a name is served by a custom template, so listings
// can say which ones are not brig's own.
func IsCustom(name string) bool {
	_, ok := custom[name]
	return ok
}

// customNames returns the custom templates in name order.
func customTemplates() []Template {
	out := make([]Template, 0, len(custom))
	for _, t := range custom {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
