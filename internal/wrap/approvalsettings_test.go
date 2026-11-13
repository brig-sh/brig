package wrap

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// notSettings are BRIG_ names in brig's source that are not settings a run
// reads from your environment.
var notSettings = map[string]string{
	"PROJECT":       "the guest variable brig sets to where the project is mounted",
	"ROUTE_":        "the prefix of the guest variables a route sets",
	"LS_KEEP_INDEX": "a setting of brig ls, which boots nothing",
}

// settingKey finds a setting where brig's source reads one: through Env, by
// the strict reader in Load, or by its whole name in a string.
var settingKey = regexp.MustCompile(`"BRIG_([A-Z][A-Z0-9_]*)"|` +
	`(?:env|e|c\.env)\.(?:String|Bool|Int|Fields|StrictBool|Get|getNamed|SettingName|setting)\("([A-Z][A-Z0-9_]*)"|` +
	`strict\("([A-Z][A-Z0-9_]*)"`)

// Every setting brig reads is a decision about the approval of a target's
// run: covered by it, or exempt with a reason. A setting added later that is
// in neither list fails here, so it cannot change what a run may do while an
// approval given before it still matches.
func TestEverySettingIsADecisionOfTheApproval(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string][]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			blob, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range settingKey.FindAllStringSubmatch(string(blob), -1) {
				key := m[1] + m[2] + m[3]
				rel, _ := filepath.Rel(root, path)
				found[key] = append(found[key], rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, must := range []string{"FORWARD_ENV", "ENV_ARGV", "RUNTIME_BIN", "CONTAINERD_RUNTIME", "IMAGE"} {
		if _, ok := found[must]; !ok {
			t.Fatalf("the scan found no %s, so it finds nothing it should", must)
		}
	}
	var undecided []string
	for key, where := range found {
		_, exempt := approvalExempt[key]
		_, other := notSettings[key]
		if !slices.Contains(approvalAgentSettings, key) && !slices.Contains(approvalGlobalSettings, key) &&
			!exempt && !other {
			undecided = append(undecided, "BRIG_"+key+" (read in "+strings.Join(where, ", ")+")")
		}
	}
	sort.Strings(undecided)
	for _, u := range undecided {
		t.Errorf("%s is neither covered by an approval nor exempt from it", u)
	}
	for _, key := range append(append([]string{}, approvalAgentSettings...), approvalGlobalSettings...) {
		if _, ok := approvalExempt[key]; ok {
			t.Errorf("BRIG_%s is both covered and exempt", key)
		}
	}
}

// A setting that took effect is in the subject by the name that won, and a
// change to its value changes the digest.
func TestTheApprovalSubjectCarriesTheSettingsThatTookEffect(t *testing.T) {
	digestHost(t)
	t.Setenv("BRIG_ENV_ARGV", "1")
	t.Setenv("BRIG_PULL", "always")
	s := subjectOf(t, approvalConfig(t))
	names := map[string]string{}
	for _, set := range s.Settings {
		names[set.Name] = set.Value
	}
	if names["BRIG_ENV_ARGV"] != "1" || names["BRIG_PULL"] != "always" {
		t.Errorf("settings %v", s.Settings)
	}
	t.Setenv("BRIG_PULL", "never")
	if now := subjectOf(t, approvalConfig(t)); ApprovalDigest(now) == ApprovalDigest(s) {
		t.Error("BRIG_PULL changed and the digest did not")
	}
}
