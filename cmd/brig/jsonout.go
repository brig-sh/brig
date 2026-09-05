package main

import (
	"encoding/json"
	"io"

	"github.com/brig-sh/brig/internal/policy"
)

// The shape every brig command's --json output takes, defined here once so the
// verbs that gain a --json flag share it rather than each inventing their own.
// #9 (brig doctor) is the first; #7 adds ls, info, agent ls and secret ls, and
// it is meant to reuse jsonDocument and jsonAPIVersion rather than write a
// second shape a consumer would have to special-case.
//
// Two rules make the shape safe to depend on, and they are the contract #7
// extends rather than the mechanism:
//
//   - Within one apiVersion, a field is only ever ADDED. None is renamed and
//     none is removed, so a consumer written against v1alpha1 keeps parsing as
//     the payload grows. A breaking change is a new apiVersion, not an edit to
//     this one.
//   - No field carries a credential VALUE -- not in the payload, not in an
//     error string. Names only. The whole tool exists to keep credentials off
//     places they can leak, and a machine-readable dump is one more such place.

// jsonAPIVersion is the apiVersion every brig command's --json output carries.
//
// It is policy.APIVersion rather than a second copy of the string: "brig.sh/
// v1alpha1" is one generation of the brig.sh API, and a policy document and a
// command's output are two parts of it, not two APIs that happen to share a
// number. Defining it once is what lets them move together when the generation
// bumps.
const jsonAPIVersion = policy.APIVersion

// Two JSON shapes, and the rule that decides which a verb prints:
//
//   - A LIST or REPORT verb prints the envelope: jsonDocument wraps the payload
//     under data, so `ls`, `info`, `agent ls`, `secret ls` and `doctor` all read
//     the same {apiVersion, kind, data} whatever they list. A list goes straight
//     under data (there is no items wrapper); a report puts its own object there.
//   - A DOCUMENT verb prints the payload bare, no envelope: `agent show`,
//     `agent export`, `agent new` and `policy show` each render one profile or
//     policy meant to be saved to a file and read back by brig, so wrapping it
//     would make the file something brig no longer imports. Those four keep the
//     bare shape they had; #7 does not touch them.
//
// The dividing line is what the output is FOR: a thing to read or pipe takes the
// envelope, a thing to write to disk stays bare.

// jsonDocument wraps a command's payload with the apiVersion that pins its
// shape and the kind that names it, both beside the payload rather than folded
// into it, so a consumer reads the two envelope fields the same way whatever
// the command.
type jsonDocument struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Data       any    `json:"data"`
}

// writeJSONDocument encodes one payload as a jsonDocument, indented for a
// person reading it on a terminal and still valid for a program parsing it.
func writeJSONDocument(w io.Writer, kind string, data any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(jsonDocument{APIVersion: jsonAPIVersion, Kind: kind, Data: data})
}
