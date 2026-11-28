// Package controlroom reports a node's sandboxes, and the records of what ran
// in them, to the NOFire AI Control Room. The Control Room puts every event of
// a sandbox on the timeline of a Brig Enterprise recording of it, so a
// reviewer sees the warden's findings and the model's tool calls beside the
// terminal session.
//
// A node reports through the Control Room's host-agent routes: R20 for its
// sandbox registry and R21 for events. The Reporter tails the files the
// node's introspection writes (the warden's findings and actions, and the
// sidecar's tool-call records), picks the records the ledger takes, redacts
// them and sends them, so the warden and the sidecar never talk to the
// Control Room themselves.
//
// The same Reporter runs wherever a sandbox does. On a laptop or a
// standalone host, Brig Enterprise's agent runs it with the host's key. On a
// Kubernetes node, the warden runs it with the node's credential. Only the
// Registry, which names the sandboxes, and the credential differ.
//
// The package is public so that the warden links it without linking brig. It
// imports nothing from brig's internal/ tree. Like api/policy/v1alpha1 it is
// alpha: docs/stability.md says it carries no compatibility promise.
package controlroom
