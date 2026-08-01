# GOCACHE is pinned because the default (~/Library/Caches on macOS) is outside
# the sandbox cairn is developed in.
export GOCACHE ?= $(HOME)/.cache/go-build
export CGO_ENABLED ?= 0

BIN := bin/cairn
PKGS := ./...

.PHONY: all build test vet fmt clean e2e

all: build

build:
	go build -trimpath -o $(BIN) ./cmd/cairn

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
