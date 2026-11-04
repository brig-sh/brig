package awscred

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestPointerModes(t *testing.T) {
	good := `{"kind":"aws","awsProfile":"dev","mode":"role","roleArn":"arn:aws:iam::000000000000:role/r"}`
	if _, err := ParsePointer([]byte(good)); err != nil {
		t.Fatalf("valid role pointer refused: %v", err)
	}
	if _, err := ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"self"}`)); err != nil {
		t.Fatalf("valid self pointer refused: %v", err)
	}
	for name, doc := range map[string]string{
		"role without roleArn": `{"kind":"aws","awsProfile":"p","mode":"role"}`,
		"self with roleArn":    `{"kind":"aws","awsProfile":"p","mode":"self","roleArn":"arn:aws:iam::0:role/r"}`,
		"unknown mode":         `{"kind":"aws","awsProfile":"p","mode":"admin"}`,
		"other kind":           `{"kind":"gcp","awsProfile":"p","mode":"self"}`,
		"unknown field":        `{"kind":"aws","awsProfile":"p","mode":"self","extra":1}`,
		"not json":             `nope`,
	} {
		if _, err := ParsePointer([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPointerRefusesFlagLikeValues(t *testing.T) {
	for name, p := range map[string]Pointer{
		"profile dash":      {Kind: "aws", AWSProfile: "-x", Mode: ModeSelf},
		"profile semicolon": {Kind: "aws", AWSProfile: "a;b", Mode: ModeSelf},
		"profile empty":     {Kind: "aws", AWSProfile: "", Mode: ModeSelf},
		"arn is a flag":     {Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "--policy"},
		"source identity":   {Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "arn:aws:iam::0:role/r", SourceIdentity: "a b"},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSessionName(t *testing.T) {
	if got := SessionName("brig-devbox-api"); got != "brig-devbox-api" {
		t.Errorf("got %q", got)
	}
	if got := SessionName(strings.Repeat("x", 100)); len(got) != 64 {
		t.Errorf("length %d, want 64", len(got))
	}
	if got := SessionName("brig-a b"); got != "brig-a-b" {
		t.Errorf("got %q", got)
	}
}

func TestSourceIdentityFrom(t *testing.T) {
	got, err := SourceIdentityFrom("arn:aws:sts::111122223333:assumed-role/AWSReservedSSO_Developer_abc/someone@example.com")
	if err != nil || got != "someone@example.com" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := SourceIdentityFrom("arn:aws:sts::1:assumed-role/R/x"); err == nil {
		t.Error("one-character tail accepted")
	}
}

func TestPointerSessionBounds(t *testing.T) {
	role := func(s int) Pointer {
		return Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeRole, RoleARN: "arn:aws:iam::0:role/r", SessionSeconds: s}
	}
	if got := role(0).Session(); got != DefaultSessionSeconds {
		t.Errorf("no session named: Session() = %d, want the default %d", got, DefaultSessionSeconds)
	}
	for _, s := range []int{MinSessionSeconds, 2700, MaxSessionSeconds} {
		if err := role(s).Validate(); err != nil {
			t.Errorf("%ds refused: %v", s, err)
		}
	}
	for _, s := range []int{-1, 900, MinSessionSeconds - 1, MaxSessionSeconds + 1, 43200} {
		if err := role(s).Validate(); err == nil {
			t.Errorf("%ds accepted", s)
		}
	}
	// A stored pointer is data, so a bad session in it is refused on read
	// too, not only at import.
	if _, err := ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r","sessionSeconds":43200}`)); err == nil {
		t.Error("a stored pointer with a 12h session was accepted")
	}
	self := Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf, SessionSeconds: 2700}
	if err := self.Validate(); err == nil {
		t.Error("self mode with a session was accepted")
	}
}

// A pointer stored before session policies existed has no policyArns, and
// must come back as one with none, and marshal as it did, so it fetches as
// before.
func TestPointerPolicyARNsRoundTrip(t *testing.T) {
	old := `{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r"}`
	p, err := ParsePointer([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	if p.PolicyARNs != nil {
		t.Errorf("PolicyARNs = %v, want nil", p.PolicyARNs)
	}
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "policyArns") {
		t.Errorf("a pointer with no session policies marshals with the key: %s", doc)
	}
	p, err = ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r","policyArns":["arn:aws:iam::0:policy/a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.PolicyARNs, []string{"arn:aws:iam::0:policy/a"}) {
		t.Errorf("PolicyARNs = %v", p.PolicyARNs)
	}
}
