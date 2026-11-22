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

// export-credentials cannot narrow a session, so a self pointer that names
// session policies would vend the profile's full keys while the pointer
// claims a narrower session. A stored pointer is data and is refused on read.
func TestPointerSelfRefusesPolicyARNs(t *testing.T) {
	for _, list := range []string{`["arn:aws:iam::0:policy/p"]`, `[]`} {
		_, err := ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"self","policyArns":` + list + `}`))
		if err == nil || !strings.Contains(err.Error(), "policyArns") {
			t.Errorf("a self pointer with policyArns %s: err = %v, want a refusal naming policyArns", list, err)
		}
	}
}

// The broker passes --policy-arns only for a non-empty list, so a stored
// role pointer with an empty one would vend the whole role. A blank or
// malformed entry would reach STS as an argument. Both are refused on read.
func TestPointerRoleRefusesAnEmptyOrMalformedPolicyARNs(t *testing.T) {
	for _, list := range []string{
		`[]`,
		`[""]`,
		`[" "]`,
		`["arn:aws:iam::0:policy/a",""]`,
		`["arn:aws:iam::0:role/r"]`,
		`["-arn:aws:iam::aws:policy/ReadOnlyAccess"]`,
		`["ReadOnlyAccess"]`,
		`["arn:aws:iam::0:policy/read,only"]`,
	} {
		_, err := ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r","policyArns":` + list + `}`))
		if err == nil || !strings.Contains(err.Error(), "policyArns") {
			t.Errorf("policyArns %s: err = %v, want a refusal naming policyArns", list, err)
		}
	}
	// An AWS managed policy is a policy ARN too.
	if _, err := ParsePointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r","policyArns":["arn:aws:iam::aws:policy/ReadOnlyAccess"]}`)); err != nil {
		t.Errorf("an AWS managed policy: %v", err)
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

// IsPointer reads a document through pointerkind.Trim, so ParsePointer does
// too: a pointer an editor saved with a byte order mark or a Unicode space
// around it is parsed, not held back as a pointer that never decodes.
func TestParsePointerReadsPastABOMAndUnicodeSpace(t *testing.T) {
	const ptr = `{"kind":"aws","awsProfile":"dev","mode":"self"}`
	for _, doc := range []string{"\ufeff" + ptr, "\u00a0" + ptr + "\u00a0"} {
		if _, err := ParsePointer([]byte(doc)); err != nil {
			t.Errorf("%q: %v", doc, err)
		}
	}
}

const scopePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"*"}]}`

func scopedPtr() Pointer {
	return Pointer{Kind: "aws", AWSProfile: "dev", Mode: ModeScoped, ScopePolicy: scopePolicy}
}

// A scoped session keeps the caller's identity, so the fields that only a
// role session has are mistakes to refuse, not values to drop.
func TestPointerScopedRefusesRoleFields(t *testing.T) {
	if err := scopedPtr().Validate(); err != nil {
		t.Fatalf("a valid scoped pointer refused: %v", err)
	}
	for name, edit := range map[string]func(*Pointer){
		"roleArn":           func(p *Pointer) { p.RoleARN = "arn:aws:iam::0:role/r" },
		"sourceIdentity":    func(p *Pointer) { p.SourceIdentity = "me@example.com" },
		"policyArns":        func(p *Pointer) { p.PolicyARNs = []string{"arn:aws:iam::0:policy/p"} },
		"empty policyArns":  func(p *Pointer) { p.PolicyARNs = []string{} },
		"no scopePolicy":    func(p *Pointer) { p.ScopePolicy = "" },
		"policy is a space": func(p *Pointer) { p.ScopePolicy = " " },
	} {
		p := scopedPtr()
		edit(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A scope policy on a role or self pointer would be dropped by assume-role
// and export-credentials, and the pointer would claim a narrower session
// than the keys have.
func TestPointerRoleAndSelfRefuseAScopePolicy(t *testing.T) {
	for _, doc := range []string{
		`{"kind":"aws","awsProfile":"p","mode":"role","roleArn":"arn:aws:iam::0:role/r","scopePolicy":"{}"}`,
		`{"kind":"aws","awsProfile":"p","mode":"self","scopePolicy":"{}"}`,
	} {
		_, err := ParsePointer([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), "scopePolicy") {
			t.Errorf("%s: err = %v, want a refusal naming scopePolicy", doc, err)
		}
	}
}

// The policy reaches the aws CLI as the value of --policy. The CLI loads a
// value that starts with file:// or fileb:// from the host, and a value that
// starts with - could be read as a flag, so a stored pointer is data that
// must hold a compact JSON object and nothing else.
func TestPointerScopePolicyMustBeACompactObject(t *testing.T) {
	for name, policy := range map[string]string{
		"file":          "file:///etc/passwd",
		"fileb":         "fileb://x",
		"flag":          "--profile",
		"array":         `[1]`,
		"string":        `"x"`,
		"not compact":   `{ "a": 1 }`,
		"trailing data": `{"a":1} {}`,
		"two objects":   `{"a":1}{}`,
		"leading space": ` {"a":1}`,
		"not json":      `{a:1}`,
		"over 2048":     `{"a":"` + strings.Repeat("x", MaxScopePolicy-7) + `"}`,
	} {
		p := scopedPtr()
		p.ScopePolicy = policy
		doc, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParsePointer(doc); err == nil {
			t.Errorf("%s: a scopePolicy of %d characters was accepted", name, len(policy))
		}
	}
	// The limit itself is accepted.
	p := scopedPtr()
	p.ScopePolicy = `{"a":"` + strings.Repeat("x", MaxScopePolicy-8) + `"}`
	if err := p.Validate(); err != nil {
		t.Errorf("a policy of exactly %d characters: %v", MaxScopePolicy, err)
	}
}

// The cache tells keys of two pointers apart with Equal, so the policy must
// survive a round trip byte for byte, and two policies must not compare
// equal.
func TestPointerScopePolicyRoundTrip(t *testing.T) {
	p := scopedPtr()
	p.ScopePolicy = `{"Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::a<b>&c/*"}]}`
	doc, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParsePointer(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Equal(p) || q.ScopePolicy != p.ScopePolicy {
		t.Errorf("round trip: %+v, want %+v", q, p)
	}
	other := scopedPtr()
	if other.Equal(p) {
		t.Error("two pointers with different scope policies are Equal")
	}
	// A pointer with no scope policy marshals as it did before the field.
	doc, err = json.Marshal(Pointer{Kind: "aws", AWSProfile: "p", Mode: ModeSelf})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), "scopePolicy") {
		t.Errorf("a pointer with no scope policy marshals with the key: %s", doc)
	}
}

func TestCompactPolicy(t *testing.T) {
	got, err := CompactPolicy([]byte("{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": []\n}\n"))
	if err != nil || got != `{"Version":"2012-10-17","Statement":[]}` {
		t.Errorf("got %q, %v", got, err)
	}
	for doc, want := range map[string]string{
		`[]`:              "must be a JSON object",
		`"x"`:             "must be a JSON object",
		`{"a":`:           "is not valid JSON",
		`{"a":1} {"b":2}`: "is not valid JSON",
		``:                "is not valid JSON",
		`{"a":"` + strings.Repeat("x", MaxScopePolicy) + `"}`: "2048",
	} {
		_, err := CompactPolicy([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.20q: err = %v, want %q", doc, err, want)
		}
	}
	// A decode error never quotes the document: it can hold an account id
	// or a bucket name the user did not mean to print.
	if _, err := CompactPolicy([]byte(`{"Resource":"arn:aws:s3:::private-bucket" oops}`)); err == nil ||
		strings.Contains(err.Error(), "private-bucket") {
		t.Errorf("err = %v", err)
	}
}

func TestPointerScopedSessionBounds(t *testing.T) {
	p := scopedPtr()
	if got := p.Session(); got != DefaultSessionSeconds {
		t.Errorf("no session named: Session() = %d, want %d", got, DefaultSessionSeconds)
	}
	for _, s := range []int{MinSessionSeconds, MaxSessionSeconds} {
		p.SessionSeconds = s
		if err := p.Validate(); err != nil {
			t.Errorf("%ds refused: %v", s, err)
		}
	}
	for _, s := range []int{MinSessionSeconds - 1, MaxSessionSeconds + 1} {
		p.SessionSeconds = s
		if err := p.Validate(); err == nil {
			t.Errorf("%ds accepted", s)
		}
	}
}
