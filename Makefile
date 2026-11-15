PREFIX ?= $(HOME)/.local
ARCH := $(shell go env GOARCH)

.PHONY: build test install clean release
build:
	mkdir -p bin
	go build -trimpath -o bin/zbox ./cmd/zbox
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -o bin/zbox-linux-$(ARCH) ./cmd/zbox

test:
	go test -race ./...
	go vet ./...
	python3 scripts/test-install.py

release:
	bash scripts/release.sh "$(VERSION)"

install: build
	mkdir -p "$(PREFIX)/bin"
	install -m 755 bin/zbox bin/zbox-linux-$(ARCH) "$(PREFIX)/bin/"

clean:
	rm -rf bin
