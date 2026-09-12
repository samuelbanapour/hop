VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)
REVISION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
  -X github.com/sammybanapour/hop/internal/core.Version=$(VERSION) \
  -X github.com/sammybanapour/hop/internal/core.Revision=$(REVISION)

.PHONY: build install test vet fmt index clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/hop ./cmd/hop

install: build
	install -d $(HOME)/.hop/bin
	install bin/hop $(HOME)/.hop/bin/hop

test:
	go test ./...

vet:
	go vet ./...
	gofmt -l .

fmt:
	gofmt -w .

# Refresh the built-in recipe index from live upstream releases.
index:
	go run ./tools/genindex -j 6

clean:
	rm -rf bin/
