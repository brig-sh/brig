package main

import (
	"context"
	"errors"
	"log"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/brig-sh/brig/internal/awscred"
	"github.com/brig-sh/brig/internal/broker"
	"github.com/brig-sh/brig/internal/injector"
)

// awsInstallHint is the refusal of a host with no aws binary. Only the user
// can install it, and the broker cannot vend without it.
const awsInstallHint = "the aws command was not found; install the AWS CLI v2: " +
	"https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"

// awsVendor is the broker.Vendor of the aws routes. It fetches through the
// host's aws CLI and holds the keys in memory, per sandbox and route.
type awsVendor struct {
	cli   awscred.CLI
	cache *awscred.Cache
	// log is the broker log. A failure the user cannot act on is written
	// there, and the guest is told to look.
	log *log.Logger

	mu sync.Mutex
	// failing holds "<sandbox>/<route>" whose last fetch failed.
	failing map[string]bool
}

var _ broker.Vendor = (*awsVendor)(nil)

func newAWSVendor(bin string, now func() time.Time, logger *log.Logger) *awsVendor {
	if now == nil {
		now = time.Now
	}
	cli := awscred.CLI{Bin: bin, Env: awsEnv(os.Environ())}
	return &awsVendor{cli: cli, cache: awscred.NewCache(cli, now), log: logger, failing: map[string]bool{}}
}

// awsEnv is the environment of the aws subprocess: the broker's own, which
// brig already cut to brokerEnv, with every AWS_ variable dropped. An
// AWS_PROFILE or AWS_ACCESS_KEY_ID that reached the broker would decide
// which identity a sandbox gets, over the pointer that names one.
func awsEnv(environ []string) []string {
	env := []string{}
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "AWS_") {
			env = append(env, kv)
		}
	}
	return env
}

// Vend answers the guest's request on route with keys that have at least 20
// minutes left.
func (v *awsVendor) Vend(ctx context.Context, sandbox, route string, doc []byte) (broker.VendedCredentials, error) {
	p, err := awscred.ParsePointer(doc)
	if err != nil {
		return broker.VendedCredentials{}, v.failed(sandbox, route, err, false)
	}
	creds, err := v.cache.Get(ctx, sandbox, route, p)
	if err != nil {
		return broker.VendedCredentials{}, v.failed(sandbox, route, err, true)
	}
	v.succeeded(sandbox, route)
	return broker.VendedCredentials{AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
		Token: creds.SessionToken, Expiration: creds.Expiration}, nil
}

// check is the registration's look at a route that vends: the pointer
// parses, the cache holds keys with 15 minutes left, and the profile names a
// region. A lapsed SSO login refuses the run that would otherwise start and
// fail at the guest's first AWS call.
func (v *awsVendor) check(ctx context.Context, sandbox, route string, doc []byte) (string, injector.VendInfo, error) {
	p, err := awscred.ParsePointer(doc)
	if err != nil {
		return "", injector.VendInfo{}, v.failed(sandbox, route, err, false)
	}
	if err := v.cache.Ensure(ctx, sandbox, route, p); err != nil {
		return "", injector.VendInfo{}, v.failed(sandbox, route, err, true)
	}
	region, err := v.cli.Region(ctx, p.AWSProfile)
	if err != nil {
		return "", injector.VendInfo{}, v.failed(sandbox, route, err, true)
	}
	v.succeeded(sandbox, route)
	return region, injector.VendInfo{Mode: string(p.Mode), AWSProfile: p.AWSProfile, RoleARN: p.RoleARN}, nil
}

// failed records the failure and returns the error to answer with. ranAWS
// is whether err came from running aws. One the user fixes on the host is a
// *broker.VendError, shown to the guest. Any other goes to the broker log and
// returns as it is, and the guest is told only to look there: its text can
// name paths of the host. awscred keeps stdout, where the keys are, out of
// every error.
func (v *awsVendor) failed(sandbox, route string, err error, ranAWS bool) error {
	v.mu.Lock()
	v.failing[sandbox+"/"+route] = true
	v.mu.Unlock()
	var sso *awscred.ErrSSOExpired
	if errors.As(err, &sso) {
		return &broker.VendError{Message: sso.Error()}
	}
	// awscred reports a failed start in words, not as exec.ErrNotFound, so
	// the binary is looked up again here, and only on a failure of aws: a
	// malformed pointer is not fixed by installing it.
	if _, lerr := exec.LookPath(v.bin()); ranAWS && lerr != nil {
		return &broker.VendError{Message: awsInstallHint}
	}
	v.log.Printf("sandbox %s: route %s: fetching AWS credentials: %v", sandbox, route, err)
	return err
}

func (v *awsVendor) succeeded(sandbox, route string) {
	v.mu.Lock()
	delete(v.failing, sandbox+"/"+route)
	v.mu.Unlock()
}

func (v *awsVendor) bin() string {
	if v.cli.Bin == "" {
		return "aws"
	}
	return v.cli.Bin
}

// forget drops the keys and the failures of a sandbox that is gone, or whose
// registration was refused.
func (v *awsVendor) forget(sandbox string) {
	v.cache.Forget(sandbox)
	v.mu.Lock()
	for k := range v.failing {
		if strings.HasPrefix(k, sandbox+"/") {
			delete(v.failing, k)
		}
	}
	v.mu.Unlock()
}

// report returns the routes whose last fetch failed, sorted.
func (v *awsVendor) report() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Sorted(maps.Keys(v.failing))
}
