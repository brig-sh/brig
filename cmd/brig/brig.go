// Command brig is Brig Community: it runs a coding agent in a sandbox. The
// command line itself is internal/cli, which Brig Enterprise links too.
package main

import "github.com/brig-sh/brig/internal/cli"

func main() { cli.Main(cli.Options{Edition: "community"}) }
