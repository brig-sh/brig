package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Vendor fetches the temporary credentials a route that vends answers with.
// The process that hosts the Handler implements it, so that this package
// does not depend on the provider's tooling.
type Vendor interface {
	// Vend returns credentials for sandbox on route. doc is the document of
	// the route's secret, as the secret store holds it.
	Vend(ctx context.Context, sandbox, route string, doc []byte) (VendedCredentials, error)
}

// VendedCredentials are temporary credentials. They live in the broker's
// memory and in the response to the guest, and nowhere else.
type VendedCredentials struct {
	AccessKeyID, SecretAccessKey, Token string
	Expiration                          time.Time
}

// VendError marks a failure that the guest's user can fix on the host, such
// as an expired login. Its Message is shown to the guest. Any other error
// from a Vendor is logged by the Vendor and answered with a fixed text, since
// its text can hold paths or the output of a tool.
type VendError struct{ Message string }

func (e *VendError) Error() string { return e.Message }

// vend answers the guest's request with credentials from the Vendor. The
// request has passed every check of a proxied route, and doc is the secret
// of the route, resolved for this sandbox. Nothing here reaches an upstream.
func (h *Handler) vend(w http.ResponseWriter, r *http.Request, a *auditLine, p routePath, doc []byte) {
	if h.vendor == nil {
		a.check = "vend"
		http.Error(w, "brig broker: route "+p.route+" vends credentials and this broker has no vendor",
			http.StatusBadGateway)
		return
	}
	creds, err := h.vendor.Vend(r.Context(), p.sandbox, p.route, doc)
	if err != nil {
		a.check = "vend"
		var ve *VendError
		if errors.As(err, &ve) {
			http.Error(w, "brig broker: route "+p.route+": "+ve.Message, http.StatusBadGateway)
			return
		}
		http.Error(w, "brig broker: the credentials of route "+p.route+" could not be fetched; see the broker log",
			http.StatusBadGateway)
		return
	}
	body, err := json.Marshal(struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      string
	}{creds.AccessKeyID, creds.SecretAccessKey, creds.Token, creds.Expiration.UTC().Format(time.RFC3339)})
	if err != nil {
		a.check = "vend"
		http.Error(w, "brig broker: internal error", http.StatusInternalServerError)
		return
	}
	a.vended = creds.AccessKeyID
	// A cache between the guest's tool and the broker would keep keys that
	// expire.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}
