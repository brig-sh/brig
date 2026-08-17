package profile

import "embed"

// builtinFS carries the profiles brig ships. They are files rather than Go
// literals because a profile is data: adding one should not mean editing Go
// and cutting a release.
//
// Embedded rather than written to the config directory on first run. That is
// deliberate and, for now, load-bearing: the deny lists exist to stop a
// metered API key silently outranking a subscription token, and a deny entry
// added in a later release has to reach installs that already exist. A copy on
// disk that brig must never overwrite could not receive one.
//
// It is also expected to be revisited, which is why nothing outside registry.go
// touches this variable -- see the source list in Load there.
//
// Wave 1 ships claude-code and codex as the proven core; gemini, grok and
// opencode are example profiles that build and run but have had less mileage.
// cursor is unpublished pending a terms check.
//
// The five published images default to :latest, which is a multi-arch index
// covering linux/arm64 and linux/amd64 -- so one reference is right on both an
// Apple Silicon Mac and an x86 Linux host, and the runtime picks the matching
// manifest. community-images also publishes :arm64 and :amd64, and an
// immutable :<arch>-<sha> pin per build; pin with --image when you want a build
// that cannot move under you.
//
//go:embed specs/*.yaml
var builtinFS embed.FS
