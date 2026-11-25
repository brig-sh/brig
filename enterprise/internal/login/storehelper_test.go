// Copyright 2026 NOFire AI. All rights reserved.
// Use of this file is governed by the NOFire AI Enterprise License in enterprise/LICENSE.

package login

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/cli"
	"github.com/brig-sh/brig/internal/secret/login"
)

// brigHelperEnv makes the test binary of this package Brig Enterprise
// itself, with the login tokens in the JSON file BRIG_TEST_LOGIN_STORE
// names. script/smoke.sh runs brig login and brig logout through it: they
// are two processes that must share one store, and the keychain of whoever
// runs the smoke test is not the test's to fill.
const brigHelperEnv = "BRIG_TEST_BRIG_HELPER"

// TestMain hands the process to brigHelper before any test runs.
func TestMain(m *testing.M) {
	if os.Getenv(brigHelperEnv) == "1" {
		brigHelper()
	}
	os.Exit(m.Run())
}

// brigHelper runs Brig Enterprise on os.Args with the file store, and exits.
func brigHelper() {
	if path := os.Getenv("BRIG_TEST_LOGIN_STORE"); path != "" {
		openLoginStore = func() (login.Store, error) { return fileLoginStore(path), nil }
		// The device flow floors its poll interval at 5 s, and the fake
		// authorization server answers at once, so the smoke test waits for
		// nothing. Test-only: brig itself has no such knob.
		loginSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	cli.Main(cli.Options{Edition: "enterprise", Commands: Commands()})
	os.Exit(0)
}

// fileLoginStore is a login.Store in a JSON file keyed by URL, so that the
// separate brig processes of the smoke test share one store, which an
// in-memory one could not give them.
type fileLoginStore string

func (f fileLoginStore) read() map[string]login.Token {
	m := map[string]login.Token{}
	if b, err := os.ReadFile(string(f)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (f fileLoginStore) write(m map[string]login.Token) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(string(f), b, 0o600)
}

func (f fileLoginStore) Save(t login.Token) error {
	m := f.read()
	m[t.URL] = t
	return f.write(m)
}

func (f fileLoginStore) Load(url string) (login.Token, error) {
	t, ok := f.read()[url]
	if !ok {
		return login.Token{}, login.ErrNotFound
	}
	return t, nil
}

func (f fileLoginStore) Delete(url string) error {
	m := f.read()
	if _, ok := m[url]; !ok {
		return login.ErrNotFound
	}
	delete(m, url)
	return f.write(m)
}
