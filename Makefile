# brig -- build, test, and a local release dry run.
BINDIR ?= $(CURDIR)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt snapshot clean claims claims-vm

all: vet test build

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINDIR)/brig ./cmd/brig
	go build -ldflags '$(LDFLAGS)' -o $(BINDIR)/brigd ./cmd/brigd

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# What CI runs before a tag can depend on it. Signing is skipped: keyless
# signing opens a browser for the OIDC flow, which is not what you want from a
# local build.
snapshot:
	HOMEBREW_TAP_GITHUB_TOKEN= goreleaser release --snapshot --clean --skip=publish,sign,sbom

# The claims suite. `claims` checks that every test named in docs/claims.md
# exists, so a renamed or deleted test that leaves a security claim undefended
# fails the build rather than the next refactor. `claims-vm` runs the claims
# that only a real VM can prove; it boots a sandbox where a runtime is present
# and skips cleanly where none is, so it is safe to run anywhere.
claims:
	./script/check-claims.sh

claims-vm:
	./script/claims-vm.sh

clean:
	rm -rf dist brig brigd
