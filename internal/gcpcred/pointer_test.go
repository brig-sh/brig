package gcpcred

import "testing"

func TestParsePointerAcceptsBothModes(t *testing.T) {
	for name, doc := range map[string]string{
		"impersonate": `{"kind":"gcloud","configuration":"default","mode":"impersonate","serviceAccount":"brig-readonly@nofire-staging.iam.gserviceaccount.com"}`,
		"self":        `{"kind":"gcloud","configuration":"default","mode":"self","account":"someone@example.com"}`,
	} {
		if _, err := ParsePointer([]byte(doc)); err != nil {
			t.Errorf("%s: valid pointer refused: %v", name, err)
		}
	}
}

func TestParsePointerRefuses(t *testing.T) {
	const sa = "brig@p.iam.gserviceaccount.com"
	// Every self document but the account cases names a valid account, so
	// each is refused for the reason its name gives.
	const acct = `,"account":"someone@example.com"`
	for name, doc := range map[string]string{
		"unknown field":            `{"kind":"gcloud","configuration":"default","mode":"self"` + acct + `,"extra":1}`,
		"trailing data":            `{"kind":"gcloud","configuration":"default","mode":"self"` + acct + `} {}`,
		"kind aws":                 `{"kind":"aws","configuration":"default","mode":"self"` + acct + `}`,
		"mode empty":               `{"kind":"gcloud","configuration":"default","mode":""}`,
		"mode role":                `{"kind":"gcloud","configuration":"default","mode":"role"}`,
		"configuration empty":      `{"kind":"gcloud","configuration":"","mode":"self"` + acct + `}`,
		"configuration dash":       `{"kind":"gcloud","configuration":"-x","mode":"self"` + acct + `}`,
		"configuration upper":      `{"kind":"gcloud","configuration":"Default","mode":"self"` + acct + `}`,
		"configuration space":      `{"kind":"gcloud","configuration":"a b","mode":"self"` + acct + `}`,
		"impersonate without sa":   `{"kind":"gcloud","configuration":"default","mode":"impersonate"}`,
		"sa is a flag":             `{"kind":"gcloud","configuration":"default","mode":"impersonate","serviceAccount":"-x@p.iam.gserviceaccount.com"}`,
		"sa is a user":             `{"kind":"gcloud","configuration":"default","mode":"impersonate","serviceAccount":"user@gmail.com"}`,
		"self with a service acct": `{"kind":"gcloud","configuration":"default","mode":"self","serviceAccount":"` + sa + `"` + acct + `}`,
		"self without an account":  `{"kind":"gcloud","configuration":"default","mode":"self"}`,
		"self account empty":       `{"kind":"gcloud","configuration":"default","mode":"self","account":""}`,
		"self account is a flag":   `{"kind":"gcloud","configuration":"default","mode":"self","account":"-x@y"}`,
		"self account has a space": `{"kind":"gcloud","configuration":"default","mode":"self","account":"a b@example.com"}`,
		"self account no domain":   `{"kind":"gcloud","configuration":"default","mode":"self","account":"someone"}`,
		"impersonate with account": `{"kind":"gcloud","configuration":"default","mode":"impersonate","serviceAccount":"` + sa + `"` + acct + `}`,
	} {
		if _, err := ParsePointer([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIsPointerRejectsARawToken(t *testing.T) {
	if IsPointer([]byte("ya29.abc")) {
		t.Error("a raw token was taken for a pointer")
	}
	if IsPointer([]byte(`{"kind":"aws","awsProfile":"p","mode":"self"}`)) {
		t.Error("an aws pointer was taken for a gcloud pointer")
	}
}

func TestIsPointerMatchesAMalformedGCloudDocument(t *testing.T) {
	for _, doc := range []string{
		`{"kind":"gcloud","mode":"bogus"}`,
		`{"kind":"gcloud","configuration":"default","mode":"self","extra":1}`,
		`{"kind":"gcloud","configuration":"default","account":"a@example.com"`,
		`{"kind":"gcloud","configuration":"default","mode":"self"}trailing`,
		`{"Kind":"gcloud","configuration":"default","mode":"bogus"}`,
		"\ufeff" + `{"kind":"gcloud","configuration":"default","mode":"self"}`,
	} {
		if !IsPointer([]byte(doc)) {
			t.Errorf("%s: not recognised as a gcloud pointer", doc)
		}
		if _, err := ParsePointer([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", doc)
		}
	}
}

// IsPointer reads a document through pointerkind.Trim, so ParsePointer does
// too: a pointer an editor saved with a byte order mark or a Unicode space
// around it is parsed, not held back as a pointer that never decodes.
func TestParsePointerReadsPastABOMAndUnicodeSpace(t *testing.T) {
	const ptr = `{"kind":"gcloud","configuration":"default","mode":"self","account":"someone@example.com"}`
	for _, doc := range []string{"\ufeff" + ptr, "\u00a0" + ptr + "\u00a0"} {
		if _, err := ParsePointer([]byte(doc)); err != nil {
			t.Errorf("%q: %v", doc, err)
		}
	}
}
