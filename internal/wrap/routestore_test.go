package wrap

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/secret"
)

// namesOnlyStore lists what it holds, and counts every read: the run checks a
// routed secret by name and never reads its value.
type namesOnlyStore struct {
	names []string
	reads *[]string
}

func (s namesOnlyStore) Read(name string) ([]byte, error) {
	*s.reads = append(*s.reads, name)
	return nil, secret.ErrNotFound
}

func (s namesOnlyStore) List() ([]secret.Secret, error) {
	var out []secret.Secret
	for _, name := range s.names {
		out = append(out, secret.Secret{Name: name})
	}
	return out, nil
}

// A routed secret the store does not list is a credential refusal that
// names the route, the secret and the command that imports it, and nothing
// else. The BROKER rows of info and plan name it. The store is listed, and
// no value is read.
func TestARoutedSecretTheStoreDoesNotHoldIsRefused(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, fakeRuntime{})
	var reads []string
	c.OpenStore = func() (creds.SecretReader, error) {
		return namesOnlyStore{names: []string{"gh-token"}, reads: &reads}, nil
	}
	err := c.routeStoreRefusal(c.checkRouteStore())
	var ce creds.CredentialError
	if !errors.As(err, &ce) || !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("refusal = %v, want a credential refusal for an absent secret", err)
	}
	for _, want := range []string{"policy brokered (route anthropic) injects", `missing secret "claude-credentials"`,
		"import it from your host with: brig secret import claude-code"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "gh-token") {
		t.Errorf("the refusal names a secret the store holds: %v", err)
	}
	if got := c.brokerLine(); !strings.Contains(got, "kept on the host: claude-credentials, gh-token; "+
		"not in brig's secret store: claude-credentials, so the run is refused; brig secret import claude-code") {
		t.Errorf("BROKER row: %q", got)
	}
	if b := c.infoBroker(); b == nil || !slices.Equal(b.Missing, []string{"claude-credentials"}) || b.Refused == "" {
		t.Errorf("info --json broker = %+v", b)
	}
	d := c.Plan()
	if d.Broker == nil || !slices.Equal(d.Broker.Missing, []string{"claude-credentials"}) ||
		!slices.Equal(d.Broker.Routes, []string{"anthropic", "github"}) || d.Broker.Refused == "" {
		t.Errorf("plan broker = %+v", d.Broker)
	}
	c.Out = &bytes.Buffer{}
	c.PrintPlan(d)
	if out := c.Out.(*bytes.Buffer).String(); !strings.Contains(out, "\nBROKER       transport ") ||
		!strings.Contains(out, "not in brig's secret store: claude-credentials, so the run is refused") {
		t.Errorf("the plan has no BROKER row naming the missing secret:\n%s", out)
	}
	if len(reads) != 0 {
		t.Errorf("the run read %v out of the store", reads)
	}
}

// A store that does not open refuses a route too. On a host with none, the
// refusal says a Secret Service is what holds the secret, and does not offer
// --from-command, which writes to the same store. Any other failure is said
// as the store said it.
func TestARouteWhoseStoreDoesNotOpenIsRefused(t *testing.T) {
	c := routedConfig(t, claudeAndGitHub, fakeRuntime{})
	c.OpenStore = func() (creds.SecretReader, error) { return nil, secret.ErrUnsupported }
	err := c.routeStoreRefusal(c.checkRouteStore())
	var ce creds.CredentialError
	if !errors.As(err, &ce) || !errors.Is(err, secret.ErrUnsupported) {
		t.Fatalf("refusal = %v, want a credential refusal for the store", err)
	}
	for _, want := range []string{"injects claude-credentials, gh-token, which the broker reads from brig's secret store",
		"Secret Service", "gnome-keyring or KWallet", "brig secret import claude-code"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--from-command") {
		t.Errorf("the refusal offers --from-command: %v", err)
	}
	if got := c.brokerLine(); !strings.Contains(got, "brig's secret store is not available on this host, so the run is refused") {
		t.Errorf("BROKER row: %q", got)
	}
	if d := c.Plan(); d.Broker == nil || d.Broker.StoreError == "" {
		t.Errorf("plan broker = %+v", d.Broker)
	}

	locked := errors.New("the keyring is locked")
	c.OpenStore = func() (creds.SecretReader, error) { return nil, locked }
	if err := c.routeStoreRefusal(c.checkRouteStore()); !errors.Is(err, locked) || !errors.As(err, &ce) ||
		strings.Contains(err.Error(), "Secret Service") {
		t.Errorf("refusal of a locked store = %v", err)
	}
}

// A run with no route opens no store for it.
func TestARunWithNoRouteOpensNoStoreForIt(t *testing.T) {
	c := envelopeConfig()
	c.OpenStore = func() (creds.SecretReader, error) {
		t.Fatal("the store was opened for a run with no route")
		return nil, nil
	}
	if err := c.routeStoreRefusal(c.checkRouteStore()); err != nil {
		t.Errorf("refusal = %v", err)
	}
	if d := c.Plan(); d.Broker != nil {
		t.Errorf("plan broker = %+v", d.Broker)
	}
}
