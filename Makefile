GO      ?= go
BINDIR  ?= /usr/local/bin

.PHONY: build build-all build-server build-admin install uninstall reinstall clean test lint

build:
	$(GO) build -o agentlink ./cmd/agentlink/

build-admin:
	$(GO) build -o agentlink-admin ./cmd/agentlink-admin/

# CGO_ENABLED=0 produces static binaries with no glibc dependency, so the
# release artifacts run on any Linux regardless of the build machine's glibc
# version (otherwise they carry the CI runner's GLIBC_2.3x symbol requirements).
build-all:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -o bin/agentlink-linux-amd64 ./cmd/agentlink/
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -o bin/agentlink-linux-arm64 ./cmd/agentlink/
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -o bin/agentlink-darwin-amd64 ./cmd/agentlink/
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -o bin/agentlink-darwin-arm64 ./cmd/agentlink/

build-server:
	$(GO) build -o server ./cmd/server/

install: build
	mkdir -p $(BINDIR)
	cp agentlink $(BINDIR)/agentlink

uninstall:
	rm -f $(BINDIR)/agentlink

reinstall: uninstall install

clean:
	rm -f agentlink server agentlink-admin
	rm -rf bin

test:
	$(GO) test ./... -count=1

lint:
	$(GO) vet ./...
