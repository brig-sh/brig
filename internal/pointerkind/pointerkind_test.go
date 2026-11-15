package pointerkind

import "testing"

// Each of these names the kind, so the broker must hold it back. A whole-
// document unmarshal refuses all but the first, and the document then passes
// for a raw credential and is sent upstream or to the guest.
func TestIsMatchesADocumentThatNamesTheKind(t *testing.T) {
	for _, doc := range []string{
		`{"kind":"aws","awsProfile":"dev","mode":"self"}`,
		// Cut short after the kind, as a truncated write leaves it.
		`{"kind":"aws","awsProfile":"dev","roleArn":"arn:aws:iam::0:role/r"`,
		`{"kind":"aws"`,
		`{"kind":"aws",`,
		// Data after the object.
		`{"kind":"aws","awsProfile":"dev","mode":"self"}x`,
		`{"kind":"aws","awsProfile":"dev","mode":"self"}{}`,
		// encoding/json matches a field name without regard to case, so
		// ParsePointer takes Kind for kind.
		`{"Kind":"aws","awsProfile":"dev","mode":"self"}`,
		`{"KIND":"AWS"}`,
		// With a duplicate key, ParsePointer keeps the last.
		`{"kind":"other","kind":"aws"}`,
		// The kind after a nested value.
		`{"policyArns":["a",{"b":1}],"kind":"aws"}`,
		"\n\t {\"kind\" : \"aws\"}",
		// A byte order mark, which an editor on Windows writes, and Unicode
		// spaces and format characters: the JSON decoder refuses each one,
		// and the document would pass for a raw credential.
		"\ufeff{\"kind\":\"aws\",\"awsProfile\":\"dev\",\"mode\":\"self\"}",
		"\u00a0{\"kind\":\"aws\",\"awsProfile\":\"dev\",\"mode\":\"self\"}",
		"\u200b\ufeff {\"kind\":\"aws\"",
	} {
		if !Is([]byte(doc), "aws") {
			t.Errorf("%s: not taken for kind aws", doc)
		}
	}
}

func TestIsRefusesADocumentThatDoesNotNameTheKind(t *testing.T) {
	for _, doc := range []string{
		"",
		"ghp_token",
		`"aws"`,
		`["aws"]`,
		`{}`,
		`{"accessToken":"x"}`,
		`{"kind":"gcloud"}`,
		`{"kind":1}`,
		`{"kind":null}`,
		// The kind only below the top level.
		`{"inner":{"kind":"aws"}}`,
		// A value of kind that is cut short names no kind.
		`{"kind":"aw`,
	} {
		if Is([]byte(doc), "aws") {
			t.Errorf("%q: taken for kind aws", doc)
		}
	}
}
