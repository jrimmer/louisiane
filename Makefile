VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/jrimmer/louisiane/internal/cli.Version=$(VERSION)
GO_BUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: build test dist clean

build:
	$(GO_BUILD) -o lal ./cmd/lal

test:
	go vet ./...
	go test -race ./...

# Release binaries and their checksums, as install.sh expects them.
dist:
	rm -rf dist && mkdir dist
	GOOS=linux GOARCH=amd64 $(GO_BUILD) -o dist/lal_linux_amd64 ./cmd/lal
	GOOS=linux GOARCH=arm64 $(GO_BUILD) -o dist/lal_linux_arm64 ./cmd/lal
	cd dist && sha256sum lal_* > checksums.txt

clean:
	rm -rf lal dist
