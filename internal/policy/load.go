package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one policy plus the file it was read from.
type Entry struct {
	Policy Policy
	Path   string
}

// reservedBasenames are the files in the policy directory that are not
// policies. The directory is flat, so every yaml/json in it would
// otherwise be read as one -- attachmentsBasename would be reported as a
// broken policy rather than read as what it actually is.
var reservedBasenames = map[string]bool{
	attachmentsBasename: true,
}

// isPolicyFile reports whether a directory entry is a policy. Anything else
// in the directory -- a README, an editor's backup, a reserved basename --
// is ignored rather than reported as broken.
func isPolicyFile(name string) bool {
	if reservedBasenames[name] {
		return false
	}
	switch filepath.Ext(name) {
	case ".yaml", ".yml", ".json":
		return true
	}
	return false
}

// LoadAll reads every policy in dir, keyed by the name: inside the file
// rather than the filename, since a file need not be named after the policy
// it declares.
//
// A directory that does not exist is not an error: most installs have none
// yet. A file that fails to parse does not stop the others from loading --
// it is collected into the returned error instead, alongside any duplicate
// name, so one typo in a policy you are not using does not hide the ones you
// are. Check the returned map even when err is non-nil: it holds everything
// that did load.
func LoadAll(dir string) (map[string]Entry, error) {
	entries := map[string]Entry{}

	names, err := readDirNames(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, nil
		}
		return nil, err
	}

	var unusable, duplicate []string
	for _, name := range names {
		if !isPolicyFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		blob, err := os.ReadFile(path)
		if err != nil {
			unusable = append(unusable, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		p, err := Parse(blob)
		if err != nil {
			unusable = append(unusable, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if prev, ok := entries[p.Name]; ok {
			duplicate = append(duplicate, fmt.Sprintf(
				"%s and %s both declare the policy %q; %s wins",
				filepath.Base(prev.Path), name, p.Name, name))
		}
		entries[p.Name] = Entry{Policy: p, Path: path}
	}

	var parts []string
	if len(unusable) > 0 {
		parts = append(parts, fmt.Sprintf("ignoring %d unusable policy file(s):\n  %s",
			len(unusable), strings.Join(unusable, "\n  ")))
	}
	if len(duplicate) > 0 {
		parts = append(parts, fmt.Sprintf("%d duplicate policy name(s):\n  %s",
			len(duplicate), strings.Join(duplicate, "\n  ")))
	}
	if len(parts) == 0 {
		return entries, nil
	}
	return entries, errors.New(strings.Join(parts, "\n"))
}

// readDirNames lists a directory's entries by name, in a stable order, so
// which file is reported as the duplicate winner does not depend on the
// filesystem's own listing order.
func readDirNames(dir string) ([]string, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
