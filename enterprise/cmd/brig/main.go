// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Command brig is Brig Enterprise: the brig command line of internal/cli,
// with the commands only this edition has.
package main

import (
	"github.com/brig-sh/brig/enterprise/internal/login"
	"github.com/brig-sh/brig/enterprise/internal/recordings"
	"github.com/brig-sh/brig/internal/cli"
)

func main() {
	cli.Main(cli.Options{
		Edition:  "enterprise",
		Commands: append(login.Commands(), recordings.Commands()...),
		Attach:   recordings.Attach,
	})
}
