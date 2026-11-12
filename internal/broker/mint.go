package broker

import (
	"context"
	"errors"
	"net/http"

	"github.com/brig-sh/brig/api/policy/v1alpha1"
)

// Minter turns the document of a gcp route's secret, a pointer at a login the
// host holds, into the access token the route injects. The process that hosts
// the Handler implements it, so that this package does not depend on the
// provider's tooling.
type Minter interface {
	// Mint returns an access token for sandbox on route. doc is the document
	// of the route's secret, as the secret store holds it. A failure the
	// guest's user can fix on the host is a *VendError; the text of any other
	// is not shown to the guest.
	Mint(ctx context.Context, sandbox, route string, doc []byte) (string, error)
}

// errGCPOnly is the refusal of a gcloud pointer on a route without services.
var errGCPOnly = errors.New("a gcloud pointer on a route without services")

// mints reports whether doc is a secret the Minter takes.
func (h *Handler) mints(doc []byte) bool { return h.mintDoc != nil && h.mintDoc(doc) }

// credential returns the value route injects from doc, its secret resolved
// for this sandbox. A secret the Minter takes is minted on a gcp route alone:
// it is a pointer at the host's login, and a route without services would
// send it upstream as the token.
func (h *Handler) credential(r *http.Request, p routePath, rt *route, cred *v1alpha1.CredentialRef,
	doc []byte) (string, error) {
	if !h.mints(doc) {
		return credentialValue(cred, doc)
	}
	if rt.services == nil {
		return "", errGCPOnly
	}
	value, err := h.minter.Mint(r.Context(), p.sandbox, p.route, doc)
	if err != nil {
		return "", &mintFailure{err}
	}
	return value, nil
}

// mintFailure is an error of the Minter, kept apart from one of
// credentialValue so that only it is answered as a failed mint.
type mintFailure struct{ err error }

func (e *mintFailure) Error() string { return e.err.Error() }
func (e *mintFailure) Unwrap() error { return e.err }

// credentialRefusal answers 502 for a credential that credential could not make.
// Only a *VendError's text reaches the guest: any other can hold a path or
// the output of gcloud.
func credentialRefusal(w http.ResponseWriter, a *auditLine, p routePath, err error) {
	a.check = "credential"
	msg := "brig broker: the credential of route " + p.route + " could not be resolved"
	var mf *mintFailure
	var ve *VendError
	switch {
	case errors.Is(err, errGCPOnly):
		a.check = "credential.gcponly"
		msg = "brig broker: the credential of route " + p.route + " is a gcloud pointer, which only a gcp route takes"
	case errors.Is(err, errCredentialMalformed):
		a.check = "credential.malformed"
		msg = "brig broker: route " + p.route + ": the secret is not valid JSON. " +
			"Store it again with brig secret update or brig secret import"
	case errors.As(err, &ve):
		msg = "brig broker: route " + p.route + ": " + ve.Message
	case errors.As(err, &mf):
		msg = "brig broker: the credential of route " + p.route + " could not be minted; see the broker log"
	}
	http.Error(w, msg, http.StatusBadGateway)
}
