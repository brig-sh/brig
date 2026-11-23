// Package awscred fetches AWS credentials for the credential broker.
//
// It runs the aws CLI as a subprocess, the way Brig runs cosign and oras, so
// the user's SSO login, profiles and role chaining stay the CLI's to resolve
// and Brig links no AWS SDK. Only brig-broker and the import command link
// this package. The run path (internal/wrap, internal/creds,
// internal/injector, cmd/brigd) never does: a run delivers what the broker
// holds and does not ask AWS for anything.
//
// The credentials it returns live in the caller's memory. No error, log line
// or argv in this package carries a SecretAccessKey or a SessionToken.
package awscred

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brig-sh/brig/internal/pointerkind"
)

// Mode is how a pointer obtains credentials.
type Mode string

const (
	// ModeRole assumes a role with the profile's identity, so the session
	// carries a name and a source identity that CloudTrail attributes.
	ModeRole Mode = "role"
	// ModeSelf exports the profile's own credentials. It is never a default:
	// those keys are the user's, not a session scoped to a sandbox.
	ModeSelf Mode = "self"
	// ModeScoped is the profile's own identity, narrowed by an inline policy
	// that STS enforces. The keys can do only what both the identity and the
	// policy allow.
	ModeScoped Mode = "scoped"
)

// Pointer is what a stored secret holds for an AWS route: where to get
// credentials, never the credentials themselves.
type Pointer struct {
	Kind           string `json:"kind"` // always "aws"
	AWSProfile     string `json:"awsProfile"`
	Mode           Mode   `json:"mode"`
	RoleARN        string `json:"roleArn,omitempty"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	// SessionSeconds is the role or scoped session lifetime, or 0 for
	// DefaultSessionSeconds. Pointers stored before this field get 0.
	SessionSeconds int `json:"sessionSeconds,omitempty"`
	// PolicyARNs are managed policies passed as session policies. The keys
	// get only what both the role and these policies allow, so the list can
	// narrow the session and never widen it.
	PolicyARNs []string `json:"policyArns,omitempty"`
	// ScopePolicy is the inline policy of a scoped session, compacted. It is
	// a string and not a json.RawMessage so that it has one empty form, and
	// compact so that the same policy is the same bytes: the cache compares
	// pointers, and two spellings of one policy would fetch twice.
	ScopePolicy string `json:"scopePolicy,omitempty"`
}

// Equal is whether p and q name the same credentials. Pointer holds a slice,
// so == does not compile. DeepEqual covers every field, so a field added
// later cannot be left out of the comparison the cache relies on.
func (p Pointer) Equal(q Pointer) bool {
	return reflect.DeepEqual(p, q)
}

// Role and scoped session bounds. A leaked key works until its session ends.
// IAM cannot cap the length below an hour (max_session_duration starts at
// 3600 and no condition key covers it), so the broker requests it.
const (
	// DefaultSessionSeconds is the session a pointer gets when it names none.
	DefaultSessionSeconds = 1800
	// MinSessionSeconds stays above getMargin, so a fetched key is served
	// for at least 5 minutes before the broker replaces it.
	MinSessionSeconds = int((getMargin + 5*time.Minute) / time.Second)
	// MaxSessionSeconds is the STS cap on a session assumed from another
	// role session, such as an SSO login.
	MaxSessionSeconds = 3600
)

// MaxScopePolicy is the STS limit on an inline session policy, in
// characters.
const MaxScopePolicy = 2048

// CompactPolicy is doc as a scope policy: a JSON object with its spaces
// removed, at most MaxScopePolicy characters. The errors never quote doc,
// which can name accounts and buckets the user did not mean to print.
func CompactPolicy(doc []byte) (string, error) {
	var b bytes.Buffer
	if json.Compact(&b, doc) != nil {
		return "", errors.New("the scope policy is not valid JSON")
	}
	if b.Len() == 0 || b.Bytes()[0] != '{' {
		return "", errors.New("the scope policy must be a JSON object")
	}
	if n := utf8.RuneCount(b.Bytes()); n > MaxScopePolicy {
		return "", fmt.Errorf("the scope policy is %d characters without spaces, and STS takes at most %d", n, MaxScopePolicy)
	}
	return b.String(), nil
}

// Session is how long the keys of p's role or scoped session work.
func (p Pointer) Session() int {
	if p.SessionSeconds == 0 {
		return DefaultSessionSeconds
	}
	return p.SessionSeconds
}

// A profile or ARN is passed to the aws CLI as an argument, so a value that
// starts with "-" would be read as a flag, and one with a shell or space
// character was never a profile name. The pointer comes from a stored secret,
// which is data and not trusted to be well formed.
var (
	profileRE  = regexp.MustCompile(`^[A-Za-z0-9_.+][A-Za-z0-9_.+-]*$`)
	roleARNRE  = regexp.MustCompile(`^arn:aws[A-Za-z-]*:iam::[0-9]*:role/[A-Za-z0-9/_+=,.@-]+$`)
	sessionRE  = regexp.MustCompile(`[^A-Za-z0-9+=,.@_-]`)
	identityRE = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{2,64}$`)
	// policyARNRE is a managed policy: the account's own or an AWS one.
	// IAM allows a comma in a policy name, but the aws shorthand the broker
	// passes splits on it, so such an entry would fail every vend. It is
	// refused here as the import refuses it.
	policyARNRE = regexp.MustCompile(`^arn:aws[A-Za-z-]*:iam::([0-9]+|aws):policy/[A-Za-z0-9/_+=.@-]+$`)
)

// ValidPolicyARN is whether s is a managed policy ARN, the only kind
// --policy-arns passes to STS.
func ValidPolicyARN(s string) bool {
	return policyARNRE.MatchString(s)
}

// IsPointer reports whether doc is a JSON object whose kind is "aws",
// whether or not it is otherwise valid, cut short or followed by other data.
// A malformed aws document must be refused as a pointer, never be taken for
// a raw credential and sent upstream: it names the user's AWS profile and
// role.
func IsPointer(doc []byte) bool {
	return pointerkind.Is(doc, "aws")
}

// ParsePointer decodes a stored pointer strictly and validates it.
func ParsePointer(doc []byte) (Pointer, error) {
	var p Pointer
	dec := json.NewDecoder(bytes.NewReader(pointerkind.Trim(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Pointer{}, fmt.Errorf("the aws pointer is not valid JSON of the expected shape: %w", err)
	}
	if dec.More() {
		return Pointer{}, errors.New("the aws pointer has data after the JSON document")
	}
	if err := p.Validate(); err != nil {
		return Pointer{}, err
	}
	return p, nil
}

// Validate reports why p cannot be used, if it cannot.
func (p Pointer) Validate() error {
	if p.Kind != "aws" {
		return fmt.Errorf("the pointer kind is %q, not \"aws\"", p.Kind)
	}
	if !profileRE.MatchString(p.AWSProfile) {
		return fmt.Errorf("the aws profile name %q is not usable: use letters, digits and _.+- and do not start with -", p.AWSProfile)
	}
	switch p.Mode {
	case ModeRole:
		if !roleARNRE.MatchString(p.RoleARN) {
			return fmt.Errorf("mode role needs a roleArn that is an IAM role ARN, got %q", p.RoleARN)
		}
		// assume-role is not passed the policy, so the keys would be the
		// whole role's while the pointer names a narrower session.
		if p.ScopePolicy != "" {
			return errors.New("mode role takes no scopePolicy")
		}
		// The broker passes --policy-arns only for a non-empty list, so an
		// empty one would vend the whole role where the import refuses it.
		// Refusing it also keeps nil the one form of "no policies", so two
		// equal pointers never take two cache identities.
		if p.PolicyARNs != nil && len(p.PolicyARNs) == 0 {
			return errors.New("policyArns is empty. Import the pointer again with brig secret import")
		}
		for _, a := range p.PolicyARNs {
			if !policyARNRE.MatchString(a) {
				return fmt.Errorf("the policyArns entry %q is not a managed policy ARN", a)
			}
		}
	case ModeSelf:
		if p.RoleARN != "" {
			return errors.New("mode self takes no roleArn")
		}
		if p.SourceIdentity != "" {
			return errors.New("mode self takes no sourceIdentity")
		}
		// Self keys expire when the profile's do; aws takes no duration.
		if p.SessionSeconds != 0 {
			return errors.New("mode self takes no sessionSeconds")
		}
		// export-credentials cannot narrow a session: the list would be
		// dropped and the profile's full keys vended. Refused on presence,
		// as the fields above are, so an empty list is refused too.
		if p.PolicyARNs != nil {
			return errors.New("mode self takes no policyArns")
		}
		// Nor can it take an inline policy, for the same reason.
		if p.ScopePolicy != "" {
			return errors.New("mode self takes no scopePolicy")
		}
	case ModeScoped:
		if p.RoleARN != "" {
			return errors.New("mode scoped takes no roleArn")
		}
		// STS keeps the caller's own source identity on a scoped session.
		if p.SourceIdentity != "" {
			return errors.New("mode scoped takes no sourceIdentity")
		}
		if p.PolicyARNs != nil {
			return errors.New("mode scoped takes no policyArns")
		}
		if p.ScopePolicy == "" {
			return errors.New("mode scoped needs a scopePolicy")
		}
		// The policy is the value of --policy. The aws CLI reads a value
		// that starts with file:// or fileb:// from the host, and one that
		// starts with - could be taken for a flag. A compact JSON object
		// starts with { and is neither.
		if c, err := CompactPolicy([]byte(p.ScopePolicy)); err != nil || c != p.ScopePolicy {
			return fmt.Errorf("the scopePolicy is not a compact JSON object of at most %d characters. "+
				"Import the pointer again with brig secret import", MaxScopePolicy)
		}
	default:
		return fmt.Errorf("the pointer mode %q is not role, self or scoped", p.Mode)
	}
	if p.SourceIdentity != "" && !identityRE.MatchString(p.SourceIdentity) {
		return fmt.Errorf("the source identity %q is not 2-64 characters of A-Za-z0-9+=,.@_-", p.SourceIdentity)
	}
	if p.SessionSeconds != 0 && (p.SessionSeconds < MinSessionSeconds || p.SessionSeconds > MaxSessionSeconds) {
		return fmt.Errorf("the session of %ds is outside %s to %s", p.SessionSeconds,
			time.Duration(MinSessionSeconds)*time.Second, time.Duration(MaxSessionSeconds)*time.Second)
	}
	return nil
}

// SessionName is the role session name for a sandbox: its id, which names the
// session in CloudTrail. AWS allows 64 bytes of [A-Za-z0-9+=,.@_-], so the
// rest is cut and replaced rather than refused: the id is Brig's own and a
// run must not fail over a character in a VM name.
func SessionName(sandboxID string) string {
	s := sessionRE.ReplaceAllString(sandboxID, "-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// SourceIdentityFrom derives the source identity from the caller's ARN: the
// last path segment, which for an SSO assumed role is the user's login. AWS
// pins that value to the session and every role chained from it, so it names
// who started the sandbox even after the role changes.
func SourceIdentityFrom(callerARN string) (string, error) {
	i := strings.LastIndexByte(callerARN, '/')
	if i < 0 {
		return "", errors.New("the caller ARN has no session name to use as a source identity; pass --source-identity")
	}
	tail := callerARN[i+1:]
	if !identityRE.MatchString(tail) {
		return "", fmt.Errorf("the caller's session name %q is not a valid source identity (2-64 characters of A-Za-z0-9+=,.@_-); pass --source-identity", tail)
	}
	return tail, nil
}
