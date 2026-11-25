module github.com/brig-sh/brig/enterprise

go 1.26.0

require (
	github.com/brig-sh/brig v0.0.0
	golang.org/x/sys v0.50.0
)

require (
	github.com/brig-sh/hull/pkg/telemetry v0.0.0-20261005010135-3e1cd5a92031 // indirect
	github.com/godbus/dbus/v5 v5.2.4 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	sigs.k8s.io/yaml v1.6.0 // indirect
)

// Brig Enterprise is built from the same revision as the core beside it.
replace github.com/brig-sh/brig => ../
