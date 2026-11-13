package main

import (
	"bytes"
	"testing"
)

// The listing of profile sessions, byte for byte. A script reads `brig ls
// --json` and a person reads the table, and sessions of a Brigfile target are
// about to join both. Neither form may change for a host that has none.
func TestTheListingOfProfileSessionsKeepsItsBytes(t *testing.T) {
	rows := []sandboxRow{
		{ref: "claude-code", name: "brig-claude-code", state: "running", workspace: "/ws/default"},
		{ref: "claude-code@refactor", name: "brig-claude-code-refactor", state: "stopped", workspace: "/ws/refactor"},
		{ref: "", name: "brig-mystery", state: "running", workspace: ""},
	}

	var doc bytes.Buffer
	if err := writeJSONDocument(&doc, "SandboxList", sandboxListData(rows, doctorRuntime{bin: "/opt/hull"})); err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{
  "apiVersion": "brig.sh/v1alpha1",
  "kind": "SandboxList",
  "data": {
    "runtime": {
      "kind": "hull",
      "bin": "/opt/hull",
      "available": true
    },
    "sandboxes": [
      {
        "ref": "claude-code",
        "agent": "claude-code",
        "sandbox": "brig-claude-code",
        "state": "running",
        "workspace": "/ws/default"
      },
      {
        "ref": "claude-code@refactor",
        "agent": "claude-code",
        "label": "refactor",
        "sandbox": "brig-claude-code-refactor",
        "state": "stopped",
        "workspace": "/ws/refactor"
      },
      {
        "sandbox": "brig-mystery",
        "state": "running"
      }
    ]
  }
}
`
	if doc.String() != wantJSON {
		t.Errorf("ls --json of profile sessions changed:\n%s\nwant:\n%s", doc.String(), wantJSON)
	}

	var table bytes.Buffer
	printSandboxes(&table, rows)
	const wantTable = "" +
		"REF                  SANDBOX                   STATE      WORKSPACE\n" +
		"claude-code          brig-claude-code          running    /ws/default\n" +
		"claude-code@refactor brig-claude-code-refactor stopped    /ws/refactor\n" +
		"-                    brig-mystery              running    \n"
	if table.String() != wantTable {
		t.Errorf("the ls table of profile sessions changed:\n%q\nwant:\n%q", table.String(), wantTable)
	}

	var refs bytes.Buffer
	printRefs(&refs, rows)
	if want := "claude-code\nclaude-code@refactor\n"; refs.String() != want {
		t.Errorf("ls -q of profile sessions changed: %q, want %q", refs.String(), want)
	}
}
