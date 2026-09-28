# brig -- build, test, and a local release dry run.
BINDIR ?= $(CURDIR)
# No version is stamped: the binary reads the tag, commit and modified flag
# the Go toolchain embeds from the checkout (see internal/buildinfo). In a
# linked git worktree the toolchain embeds none, so git's own answers are
# passed as well; the binary reads them only when the toolchain's are missing.
BUILDINFO := github.com/brig-sh/brig/internal/buildinfo
LDFLAGS := -s -w \
	-X $(BUILDINFO).gitCommit=$(shell git rev-parse HEAD 2>/dev/null) \
	-X $(BUILDINFO).gitCommitTime=$(shell git log -1 --format=%cI 2>/dev/null) \
	-X $(BUILDINFO).gitDescribe=$(shell git describe --tags --long --match 'v[0-9]*' 2>/dev/null) \
	-X $(BUILDINFO).gitModified=$(shell test -z "$$(git status --porcelain 2>/dev/null)" || echo true)

.PHONY: all build test vet fmt snapshot notes claims claims-vm clean netprobe

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

# The network probe the conformance suite copies into a guest, one binary per
# guest architecture, at $(NETPROBE_DIR)/linux-<arch>/netprobe. Static and
# standard library only, so it runs on any guest image whatever libc it
# carries. See test/netprobe.
NETPROBE_DIR ?= dist/netprobe
netprobe:
	for arch in arm64 amd64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '-s -w' \
			-o $(NETPROBE_DIR)/linux-$$arch/netprobe ./test/netprobe || exit 1; \
	done

# What CI runs before a tag can depend on it. Signing is skipped: keyless
# signing opens a browser for the OIDC flow, which is not what you want from a
# local build.
snapshot:
	HOMEBREW_TAP_GITHUB_TOKEN= goreleaser release --snapshot --clean --skip=publish,sign,sbom

# The release notes CI would publish for TAG, rendered from cliff.toml before
# the tag exists. Read them before you push: a subject that reads badly is
# cheap to fix while the commit is still unpushed. Needs git-cliff
# (brew install git-cliff).
notes:
	@test -n "$(TAG)" || { echo "usage: make notes TAG=v0.1.0-rc19" >&2; exit 2; }
	git cliff --unreleased --tag $(TAG)

# docs/claims.md ties each promise in docs/security.md to its tests. The
# self-test runs first, so a guard that stopped refusing bad rows fails here
# before the real table can pass through it.
claims:
	./script/check-claims.sh --self-test
	./script/check-claims.sh

# The claims only a booted sandbox proves, run against the brig this checkout
# builds. The self-test runs the checks against a fake guest first, so a
# check that stopped failing on a leak fails here before anything boots.
# Skips with no hull or nerdctl on PATH, and fails there under
# BRIG_CLAIMS_VM=require.
claims-vm: build
	./script/claims-vm.sh --self-test
	BRIG=$(BINDIR)/brig ./script/claims-vm.sh

clean:
	rm -rf dist brig brigd
