package main

import (
	"os"
	"strings"

	"github.com/brig-sh/brig/internal/secret"
)

// brigHelperEnv makes the test binary of this package brig itself, with a
// store in memory that holds the secrets BRIG_TEST_SECRETS names, or with
// no store at all under BRIG_TEST_NO_STORE=1. script/smoke.sh runs its
// routed runs through it: brig refuses a run whose route's secret is not in
// the store, and the store of whoever runs the smoke test is not the test's
// to fill. TestMain hands it the process before any test runs.
const brigHelperEnv = "BRIG_TEST_BRIG_HELPER"

// brigHelper runs brig on os.Args with the store in memory, and exits.
func brigHelper() {
	f := &fakeStore{items: map[string][]byte{}, provenance: map[string]secret.Provenance{}}
	for _, name := range strings.Fields(os.Getenv("BRIG_TEST_SECRETS")) {
		f.seed(name, "{}")
	}
	openStore = func() (secret.Store, error) {
		if os.Getenv("BRIG_TEST_NO_STORE") == "1" {
			return nil, secret.ErrUnsupported
		}
		return f, nil
	}
	main()
	os.Exit(0)
}
