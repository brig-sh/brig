package login

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brig-sh/brig/internal/secret"
	"github.com/brig-sh/brig/internal/secret/internal/keyring"
)

// fake is an in-memory keyring.Store that also records the configuration it
// was opened with and every account it was asked to touch.
type fake struct {
	keyring.Store
	cfg      keyring.Config
	items    map[string][]byte
	accounts []string
}

func (f *fake) Kind() string { return "fake" }

func (f *fake) Create(name string, v []byte) error {
	f.accounts = append(f.accounts, name)
	if _, ok := f.items[name]; ok {
		return keyring.ErrExists
	}
	f.items[name] = append([]byte(nil), v...)
	return nil
}

func (f *fake) Read(name string) ([]byte, error) {
	v, ok := f.items[name]
	if !ok {
		return nil, keyring.ErrNotFound
	}
	return v, nil
}

func (f *fake) Update(name string, v []byte) error {
	f.accounts = append(f.accounts, name)
	if _, ok := f.items[name]; !ok {
		return keyring.ErrNotFound
	}
	f.items[name] = append([]byte(nil), v...)
	return nil
}

func (f *fake) Delete(name string) error {
	if _, ok := f.items[name]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.items, name)
	return nil
}

func open(t *testing.T) (Store, *fake) {
	t.Helper()
	f := &fake{items: map[string][]byte{}}
	old := openKeyring
	openKeyring = func(c keyring.Config) (keyring.Store, error) {
		f.cfg = c
		return f, nil
	}
	t.Cleanup(func() { openKeyring = old })
	s, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	return s, f
}

func TestSaveTwiceReplaces(t *testing.T) {
	s, _ := open(t)
	a := Token{URL: "https://my.nofire.ai", AccessToken: "first", ExpiresAt: time.Unix(100, 0).UTC()}
	b := a
	b.AccessToken = "second"
	for _, tok := range []Token{a, b} {
		if err := s.Save(tok); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load(a.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "second" || !got.ExpiresAt.Equal(a.ExpiresAt) {
		t.Errorf("Load = %+v, want the second token", got)
	}
}

func TestTwoDeploymentsCoexist(t *testing.T) {
	s, _ := open(t)
	u1, u2 := "https://one.example", "https://two.example"
	for _, u := range []string{u1, u2} {
		if err := s.Save(Token{URL: u, AccessToken: "tok-" + u}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(u2); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(u1); err != nil || got.AccessToken != "tok-"+u1 {
		t.Errorf("Load(u1) = %+v, %v; want it kept", got, err)
	}
	if _, err := s.Load(u2); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(u2) = %v, want ErrNotFound", err)
	}
	if err := s.Delete(u2); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestAccountIsAValidName(t *testing.T) {
	for _, u := range []string{"https://my.nofire.ai", "https://" + strings.Repeat("a", 292)} {
		a := Account(u)
		if err := secret.ValidName(a); err != nil {
			t.Errorf("Account(%.30q...) = %q: %v", u, a, err)
		}
		if !strings.HasPrefix(a, "r") || len(a) != 65 {
			t.Errorf("Account(%.30q...) = %q, want r plus 64 hex digits", u, a)
		}
	}
}

func TestValueNeverInLabelOrAccount(t *testing.T) {
	s, f := open(t)
	const url, tok = "https://my.nofire.ai", "s3cr3t-token-value"
	if err := s.Save(Token{URL: url, AccessToken: tok}); err != nil {
		t.Fatal(err)
	}
	if len(f.accounts) == 0 || f.cfg.Label == "" {
		t.Fatalf("recorded accounts %v and label %q; want both set", f.accounts, f.cfg.Label)
	}
	for _, seen := range append(f.accounts, f.cfg.Label, f.cfg.Service) {
		if strings.Contains(seen, tok) || strings.Contains(seen, "nofire") {
			t.Errorf("%q names the URL or the token", seen)
		}
	}
}

// An item under a deployment's account that names another URL was not
// written by Save for that deployment. Load refuses it, and not as
// ErrNotFound, so logout removes it instead of reporting no login.
func TestLoadRefusesAnItemForAnotherDeployment(t *testing.T) {
	s, f := open(t)
	const url = "https://my.nofire.ai"
	f.items[Account(url)] = []byte(`{"url":"https://evil.example","access_token":"planted"}`)
	got, err := s.Load(url)
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Load = %+v, %v; want an error that is not ErrNotFound", got, err)
	}
	if got.AccessToken != "" || strings.Contains(err.Error(), "planted") || strings.Contains(err.Error(), "evil") {
		t.Errorf("Load = %+v, %q; want no token and no text from the item", got, err)
	}
}

// The trust store a person accepted at login is stored with the token, and a
// record written without the field loads with none.
func TestTheAcceptedTrustStoreIsStoredWithTheToken(t *testing.T) {
	s, f := open(t)
	url := "https://my.example"
	want := Token{URL: url, AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
		Trust: map[string]string{"SSL_CERT_FILE": "/etc/ca.pem"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(url)
	if err != nil || got.Trust["SSL_CERT_FILE"] != "/etc/ca.pem" || len(got.Trust) != 1 {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	// An item written before the field existed.
	f.items[Account(url)] = []byte(`{"url":"` + url + `","access_token":"tok","expires_at":"2099-01-01T00:00:00Z"}`)
	got, err = s.Load(url)
	if err != nil || got.Trust != nil {
		t.Fatalf("an older item loaded as %+v, %v; want no trust", got, err)
	}
}
