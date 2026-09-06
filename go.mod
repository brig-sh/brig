module github.com/brig-sh/brig

// 1.25 for the os.Root methods the workspace is written through -- ReadFile,
// WriteFile, MkdirAll, Rename, Symlink, Chmod, Readlink. Confining every
// host-side write into the sandbox's home to a root is what stops a symlink
// planted by the guest from aiming brig at a host path; see internal/wrap/rootio.go.
go 1.25.0

require sigs.k8s.io/yaml v1.6.0

require (
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sys v0.47.0 // indirect
)
