// Package v1alpha1 holds the brig.sh/v1alpha1 policy document as data, with
// its decoder and its validation.
//
// The package is public so that a later Kubernetes operator can link the
// types without linking brig. It does no I/O, and it imports nothing from
// brig's internal/ tree. internal/policy imports this package, never the
// reverse.
//
// The shape follows the CRD conventions. Optional structs are pointers with
// omitempty, maps are map[string]string, and enums are typed strings. The
// metadata field names match the Kubernetes ObjectMeta JSON. The name status
// is reserved for a CRD status subresource, so no field under spec uses it.
//
// The package is alpha. docs/stability.md says that it carries no
// compatibility promise.
package v1alpha1
