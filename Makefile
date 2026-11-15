PREFIX ?= $(HOME)/.local
ARCH := $(shell go env GOARCH)

.PHONY: build test install clean
build:
	mkdir -p bin
	go build -trimpath -o bin/agents ./cmd/agents
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -o bin/agents-linux-$(ARCH) ./cmd/agents

test:
	go test -race ./...
	go vet ./...

install: build
	mkdir -p "$(PREFIX)/bin"
	install -m 755 bin/agents bin/agents-linux-$(ARCH) "$(PREFIX)/bin/"

clean:
	rm -rf bin
