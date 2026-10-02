VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 linux/arm linux/386 linux/mips linux/mipsle linux/riscv64 linux/s390x

.PHONY: build test dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ss-server ./cmd/ss-server
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ss-agent ./cmd/ss-agent

test:
	go vet ./...
	go test -race ./...

dist:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/ss-server-$$os-$$arch ./cmd/ss-server || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/ss-agent-$$os-$$arch ./cmd/ss-agent || exit 1; \
	done

clean:
	rm -rf bin dist
