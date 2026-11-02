package wrap

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brig-sh/brig/internal/creds"
	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/verify"
)

const certNow = int64(1755436980000)

const dayMillis = int64(24 * 60 * 60 * 1000)

const certPolicy = `apiVersion: brig.sh/v1alpha1
kind: Policy
metadata: {name: brokered}
spec:
  routes:
    - name: anthropic
      preset: claude-code
      credential: {secretRef: {name: claude-credentials}}
    - name: inventory
      upstream: https://inventory.internal.example.com
      inject:
        tls:
          clientCertificate: {secretRef: {name: inventory-client}}
      guest:
        env: {INVENTORY_URL: "{route.url}"}
`

// expiryStore lists every secret with a recorded expiry, and counts reads.
type expiryStore struct {
	expiry map[string]int64
	reads  *int
}

func (s expiryStore) Read(string) ([]byte, error) {
	*s.reads++
	return nil, secret.ErrNotFound
}

func (s expiryStore) List() ([]secret.Secret, error) {
	var out []secret.Secret
	for name, exp := range s.expiry {
		out = append(out, secret.Secret{Name: name, Provenance: secret.Provenance{ExpiresAt: exp}})
	}
	return out, nil
}

func certConfig(t *testing.T, certExpiresAt int64) (*Config, *int) {
	return certConfigFor(t, certPolicy, certExpiresAt)
}

func certConfigFor(t *testing.T, policy string, certExpiresAt int64) (*Config, *int) {
	t.Helper()
	old := nowMilli
	nowMilli = func() int64 { return certNow }
	t.Cleanup(func() { nowMilli = old })
	t.Setenv("BRIG_HYPERVISOR", "hvi")
	c := routedConfig(t, policy, tagHull{&hullRecorder{}})
	shortStateDir(t)
	c.Verify = verify.Off
	c.broker = &fakeBroker{}
	reads := new(int)
	st := expiryStore{expiry: map[string]int64{"claude-credentials": 0, "inventory-client": certExpiresAt}, reads: reads}
	c.OpenStore = func() (creds.SecretReader, error) { return st, nil }
	return c, reads
}

func errOut(c *Config) string { return c.Err.(*bytes.Buffer).String() }

func TestPlanWarnsOfAClientCertificateExpiringWithinThreeDays(t *testing.T) {
	c, reads := certConfig(t, certNow+2*dayMillis)
	before := c.Plan()
	out := errOut(c)
	if n := strings.Count(out, "the client certificate inventory-client (route inventory) expires in 2d"); n != 1 {
		t.Fatalf("want one warning, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "brig secret update inventory-client --type tls") {
		t.Errorf("no renewal command:\n%s", out)
	}
	if *reads != 0 {
		t.Errorf("the plan read %d values", *reads)
	}
	// The warning is a side message: the document itself does not carry it.
	// Same configuration, a certificate far from expiry: no warning, same plan.
	c.Err = &bytes.Buffer{}
	c.OpenStore = func() (creds.SecretReader, error) {
		return expiryStore{expiry: map[string]int64{"claude-credentials": 0, "inventory-client": certNow + 20*dayMillis}, reads: reads}, nil
	}
	c2 := c
	if after := c2.Plan(); after.Digest != before.Digest {
		t.Errorf("the digest depends on the expiry: %s vs %s", before.Digest, after.Digest)
	}
	if out := errOut(c2); strings.Contains(out, "client certificate") {
		t.Errorf("a certificate with 20 days left warned:\n%s", out)
	}
}

func TestRunStartWarnsOnceOfAnExpiringClientCertificate(t *testing.T) {
	c, _ := certConfig(t, certNow-dayMillis)
	err := c.EnsureRunning(creds.Set{})
	t.Logf("EnsureRunning: %v", err)
	out := errOut(c)
	if n := strings.Count(out, "the client certificate inventory-client (route inventory) expired 1d ago"); n != 1 {
		t.Fatalf("want one warning, got %d:\n%s", n, out)
	}
	c, _ = certConfig(t, certNow-dayMillis)
	if _, err := c.BuildEnv(); err != nil {
		t.Log(err)
	}
	if strings.Contains(errOut(c), "client certificate") {
		t.Errorf("BuildEnv warned:\n%s", errOut(c))
	}
}

// Routes that present one secret share its expiry, and the warning names every
// one of them, not the last.
func TestTheWarningNamesEveryRouteThatSharesTheCertificate(t *testing.T) {
	policy := certPolicy + `    - name: audit
      upstream: https://audit.internal.example.com
      inject:
        tls:
          clientCertificate: {secretRef: {name: inventory-client}}
      guest:
        env: {AUDIT_URL: "{route.url}"}
    - name: billing
      upstream: https://billing.internal.example.com
      inject:
        tls:
          clientCertificate: {secretRef: {name: other-client}}
      guest:
        env: {BILLING_URL: "{route.url}"}
`
	c, _ := certConfigFor(t, policy, certNow+dayMillis)
	c.Plan()
	out := errOut(c)
	if n := strings.Count(out, "the client certificate inventory-client (routes audit, inventory) expires in 1d"); n != 1 {
		t.Fatalf("want one warning naming both routes, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "other-client") {
		t.Errorf("a certificate with no recorded expiry warned:\n%s", out)
	}
}

func TestNoWarningForACertificateWithNoRecordedExpiry(t *testing.T) {
	c, _ := certConfig(t, 0)
	c.Plan()
	_ = c.EnsureRunning(creds.Set{})
	if out := errOut(c); strings.Contains(out, "client certificate") {
		t.Errorf("warned without a recorded expiry:\n%s", out)
	}
}
