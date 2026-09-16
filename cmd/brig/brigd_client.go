package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/brig-sh/brig/internal/buildinfo"
)

// askBrigdBuild asks the daemon on socket which build it is, over the version
// op of its protocol. Read-only: the daemon answers from what it already knows
// about itself and touches no sandbox.
//
// The wire shape is brigd's own Response, read here by the fields the version
// op fills. A field brigd does not send stays empty, which is also what a
// daemon older than these fields sends, so an old daemon is one with no
// commit rather than a parse error.
func askBrigdBuild(socket string) (buildinfo.Info, error) {
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return buildinfo.Info{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return buildinfo.Info{}, err
	}
	if _, err := fmt.Fprintln(conn, `{"v":1,"op":"version"}`); err != nil {
		return buildinfo.Info{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return buildinfo.Info{}, err
	}
	var resp struct {
		OK         bool   `json:"ok"`
		Error      string `json:"error"`
		Version    string `json:"version"`
		Commit     string `json:"commit"`
		CommitTime string `json:"commitTime"`
		Modified   bool   `json:"modified"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return buildinfo.Info{}, fmt.Errorf("not a brigd response: %w", err)
	}
	if !resp.OK {
		return buildinfo.Info{}, errors.New(resp.Error)
	}
	info := buildinfo.Info{Version: resp.Version, Commit: resp.Commit, Modified: resp.Modified}
	if t, err := time.Parse(time.RFC3339, resp.CommitTime); err == nil {
		info.CommitTime = t
	}
	return info, nil
}
