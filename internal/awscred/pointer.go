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
	"regexp"
	"strings"
	"time"
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
)

// Pointer is what a stored secret holds for an AWS route: where to get
// credentials, never the credentials themselves.
type Pointer struct {
	Kind           string `json:"kind"` // always "aws"
	AWSProfile     string `json:"awsProfile"`
	Mode           Mode   `json:"mode"`
	RoleARN        string `json:"roleArn,omitempty"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	// SessionSeconds is the role session lifetime, or 0 for
	// DefaultSessionSeconds. Pointers stored before this field get 0.
	SessionSeconds int `json:"sessionSeconds,omitempty"`
}

// Role session bounds. A leaked key works until its session ends. IAM cannot
// cap the length below an hour (max_session_duration starts at 3600 and no
// condition key covers it), so the broker requests it.
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

// Session is how long the keys of p's role session work.
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
)

// ParsePointer decodes a stored pointer strictly and validates it.
func ParsePointer(doc []byte) (Pointer, error) {
	var p Pointer
	dec := json.NewDecoder(bytes.NewReader(doc))
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
	default:
		return fmt.Errorf("the pointer mode %q is not role or self", p.Mode)
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
