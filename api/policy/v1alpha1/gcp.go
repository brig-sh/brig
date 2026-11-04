package v1alpha1

import (
	"slices"
	"strings"
)

// GCPEndpointOverridePrefix starts the name of each gcloud setting that
// points one API at a different endpoint. brig writes these from a gcp
// route's services, so a route's guest.env never names one.
const GCPEndpointOverridePrefix = "CLOUDSDK_API_ENDPOINT_OVERRIDES_"

// GCPCACertsVar names the gcloud setting for the CA bundle it trusts. gcloud
// reads none of the usual CA variables, so brig writes this one beside the
// endpoint overrides.
const GCPCACertsVar = "CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE"

// GCPEndpointOverrideName returns the gcloud setting that points service at
// another endpoint. The injector writes it and the policy checks refuse a
// guest.env entry of that name, so both derive it here and cannot disagree.
func GCPEndpointOverrideName(service string) string {
	return GCPEndpointOverridePrefix + strings.ToUpper(service)
}

// GCPGuestVars returns the names of the guest variables brig writes for a
// route with services: one endpoint override for each, and GCPCACertsVar.
func GCPGuestVars(services []Service) []string {
	names := make([]string, 0, len(services)+1)
	for _, svc := range services {
		names = append(names, GCPEndpointOverrideName(svc.Name))
	}
	return append(names, GCPCACertsVar)
}

// gcpServicePaths maps each service that brig wires to the path that gcloud
// puts after the endpoint override for it. gcloud appends the request path
// to the override as it is, so a service whose API lives under a version
// prefix needs that prefix in the override, and the others need none. A
// wrong entry sends the request to a path the API does not serve.
var gcpServicePaths = map[string]string{
	"storage":              "storage/v1/",
	"cloudresourcemanager": "",
	"compute":              "compute/v1/",
	"logging":              "",
	"iam":                  "",
}

// GCPServicePath returns the path that the gcloud endpoint override of a
// service ends with, and whether brig wires the service.
func GCPServicePath(name string) (path string, ok bool) {
	path, ok = gcpServicePaths[name]
	return path, ok
}

// GCPServiceNames returns the services that brig wires, sorted.
func GCPServiceNames() []string {
	names := make([]string, 0, len(gcpServicePaths))
	for name := range gcpServicePaths {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Service is one Google API that a gcp route reaches. Each service has its
// own request policy, because the paths of one API mean nothing to another.
type Service struct {
	// Name is required. It is one of the services brig wires, and is listed
	// once in a route.
	Name string `json:"name"`
	// Request is optional. Without it, every request to the service is
	// allowed. Only methods and paths apply, as repos belongs to the
	// github-token preset.
	Request *RequestPolicy `json:"request,omitempty"`
}
