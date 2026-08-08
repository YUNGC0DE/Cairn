# GOCACHE is pinned because the default (~/Library/Caches on macOS) is outside
# the sandbox cairn is developed in.
export GOCACHE ?= $(HOME)/.cache/go-build
export CGO_ENABLED ?= 0

# The binary is git-cairn so that git finds it as a subcommand (`git cairn show …`).
# The cairn symlink next to it keeps the short form working.
BIN := bin/git-cairn
PKGS := ./...
PREFIX ?= /usr/local

.PHONY: all build install uninstall test vet fmt clean e2e

all: build

build:
	go build -trimpath -o $(BIN) ./cmd/git-cairn
	ln -sf git-cairn bin/cairn

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN) $(PREFIX)/bin/git-cairn
	ln -sf git-cairn $(PREFIX)/bin/cairn

uninstall:
	rm -f $(PREFIX)/bin/git-cairn $(PREFIX)/bin/cairn

vet:
	go vet $(PKGS)

test:
	go test $(PKGS)

fmt:
	gofmt -l -w .

e2e: build
	./scripts/e2e.sh

clean:
	rm -rf bin dist
