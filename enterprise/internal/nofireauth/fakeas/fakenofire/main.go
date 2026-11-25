// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

// Command fakenofire runs the fake NOFire AI authorization server, so that a
// developer can try brig login by hand in a browser.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/brig-sh/brig/enterprise/internal/nofireauth/fakeas"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "address to listen on")
	approve := flag.String("approve", "page", "how to answer a sign-in: auto, deny or page")
	maxTTL := flag.Duration("max-ttl", 720*time.Hour, "longest token lifetime the server grants")
	flag.Parse()

	o := fakeas.Options{MaxTTL: *maxTTL, Log: func(line string) { log.Print(line) }}
	switch *approve {
	case "auto":
		o.Approval = fakeas.Auto
	case "deny":
		o.Approval = fakeas.Deny
	case "page":
		o.Approval = fakeas.Page
	default:
		fmt.Fprintf(os.Stderr, "fakenofire: -approve must be auto, deny or page, not %q\n", *approve)
		os.Exit(2)
	}
	s, err := fakeas.Start(*addr, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakenofire:", err)
		os.Exit(1)
	}
	defer s.Close()
	fmt.Printf("export BRIG_NOFIRE_URL=%s\n", s.URL)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
}
