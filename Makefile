VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet release clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

test:
	go test ./...

vet:
	go vet ./...

release:
	VERSION=$(VERSION) ./scripts/build.sh

clean:
	rm -rf bin dist
