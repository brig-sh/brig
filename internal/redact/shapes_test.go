package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

// Inputs a reviewer found that came back unchanged, or with the secret still
// readable. Each planted value must be gone.
func TestRedactReviewedLeaks(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("aB3", 12)
	secret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	tok := brigToken(t)
	for _, c := range []struct {
		line    string
		planted []string
	}{
		{"refused token " + tok + ".", []string{tok[len(tok)-43:]}},
		{"refused token " + tok + "...", []string{tok[len(tok)-43:]}},
		{"refused token " + tok + ".log", []string{tok[len(tok)-43:]}},
		{"BRIG_AWS_KEY=AKIAABCDEFGHIJKLMNOP:" + secret, []string{"AKIAABCDEFGHIJKLMNOP", secret}},
		{`{"token":"` + ghp + ` extra"}`, []string{ghp, "extra"}},
		{`API_KEY="` + ghp + `:password"`, []string{ghp, "password"}},
		{"PASSWORD=hunter2hunter2", []string{"hunter2hunter2"}},
		{"TOKEN=abc123xyz", []string{"abc123xyz"}},
		{"SECRET=s3cr3tvalue", []string{"s3cr3tvalue"}},
		{"KEY=k3yvalue", []string{"k3yvalue"}},
		{vendDocument(t, "AKIAABCDEFGHIJKLMNOP", secret, "IQoJb3JpZ2luX2VjEXAMPLETOKEN"), []string{"AKIAABCDEFGHIJKLMNOP", secret, "IQoJb3JpZ2luX2VjEXAMPLETOKEN"}},
		{`body={\"accessToken\":\"opaque-value-1\",\"refresh_token\":\"opaque-value-2\"}`, []string{"opaque-value-1", "opaque-value-2"}},
		{`export DB_PASSWORD="p4ss\"word-tail-secret"`, []string{"p4ss", "word-tail-secret"}},
		{"aws_secret_access_key = " + secret, []string{secret}},
		{"Authorization: Basic dXNlcjpwYXNz", []string{"dXNlcjpwYXNz"}},
		{"map[Authorization:[token abcdef0123456789abcdef]]", []string{"abcdef0123456789abcdef"}},
		{"KEY=[masked:x]realsecret", []string{"realsecret"}},
	} {
		got := String(c.line)
		for _, p := range c.planted {
			if strings.Contains(got, p) {
				t.Errorf("%q still holds %q after masking: %q", c.line, p, got)
			}
		}
		if twice := String(got); twice != got {
			t.Errorf("masking %q twice gave %q, once gave %q", c.line, twice, got)
		}
	}
}

// A value inside a name's value is masked once, as the name's value, not
// as a mask with the rest of the original around it.
func TestRedactMasksANamedValueWhole(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("aB3", 12)
	if got, want := String("GH_TOKEN=prefix-"+ghp), "GH_TOKEN=[masked: env-secret]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The rules are tested above with the shapes they know. This test plants
// random values the way programs print them, so a template the rules do not
// handle fails here, whatever the value looks like.
func TestRedactMasksPlantedValues(t *testing.T) {
	envNames := []string{"GH_TOKEN", "TOKEN", "API_KEY", "KEY", "SECRET", "DB_PASSWORD", "PASSWD",
		"aws_secret_access_key", "aws_session_token", "client_secret"}
	jsonNames := []string{"token", "Token", "accessToken", "refresh_token", "SecretAccessKey",
		"SessionToken", "client_secret", "apiKey", "api_key", "password", "privateKey"}
	envTemplates := []string{"%s=%s", "%s = %s", `%s="%s"`, "%s='%s'", `export %s="%s"`,
		"%s=%s.", "%s=%s,", `%s="%s`, "env %s=%s more"}
	jsonTemplates := []string{`{"%s":"%s"}`, `{"%s": "%s", "x":1}`, `{\"%s\":\"%s\"}`, `{"%s":"%s`}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 400; i++ {
		v := randomValue(rng)
		var line string
		if i%2 == 0 {
			tpl := envTemplates[rng.IntN(len(envTemplates))]
			line = sprintf(tpl, envNames[rng.IntN(len(envNames))], v)
		} else {
			tpl := jsonTemplates[rng.IntN(len(jsonTemplates))]
			line = sprintf(tpl, jsonNames[rng.IntN(len(jsonNames))], v)
		}
		got := String(line)
		if strings.Contains(got, v) || strings.Contains(got, v[len(v)/2:]) {
			t.Errorf("%q masked to %q, which still holds the value", line, got)
		}
	}
}

func sprintf(tpl, name, value string) string {
	return strings.Replace(strings.Replace(tpl, "%s", name, 1), "%s", value, 1)
}

// randomValue is 20 to 64 bytes of base64, base64url or hex: the encodings
// credentials come in.
func randomValue(rng *rand.Rand) string {
	b := make([]byte, 20+rng.IntN(45))
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	switch rng.IntN(3) {
	case 0:
		return base64.StdEncoding.EncodeToString(b)
	case 1:
		return base64.RawURLEncoding.EncodeToString(b)
	default:
		return hex.EncodeToString(b)
	}
}

// brigToken builds a session token the way internal/broker/token.go's mint
// does: base64url(JSON claims) "." base64url(HMAC-SHA256(key, claims)). The
// broker does not export mint, and a registered sandbox is more than this
// test needs.
func brigToken(t *testing.T) string {
	t.Helper()
	blob, err := json.Marshal(struct {
		Sandbox string `json:"sandbox"`
		Session string `json:"session"`
		Route   string `json:"route"`
		Exp     int64  `json:"exp"`
		Typ     string `json:"typ"`
	}{"brig-claude-code", "s1", "anthropic", time.Now().Add(time.Hour).Unix(), "brig-route"})
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte(strings.Repeat("k", 32)))
	m.Write(blob)
	enc := base64.RawURLEncoding
	return enc.EncodeToString(blob) + "." + enc.EncodeToString(m.Sum(nil))
}

// vendDocument is the body internal/broker/vend.go serves on an aws route.
func vendDocument(t *testing.T, keyID, secret, token string) string {
	t.Helper()
	body, err := json.Marshal(struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      string
	}{keyID, secret, token, time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
