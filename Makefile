# Same checks as CI; `make` runs them all.
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: check fmt vet test vuln build cross image
check: fmt vet test vuln build

fmt:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

vet:
	go vet ./...

test:
	go test -race ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" ./cmd/itmo-widgets-agent

cross:
	@for t in $(TARGETS); do os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  echo "dist/itmo-widgets-agent-$$os-$$arch$$ext"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
	    -o dist/itmo-widgets-agent-$$os-$$arch$$ext ./cmd/itmo-widgets-agent || exit 1; done

image:
	docker build --build-arg VERSION=$(VERSION) -t itmo-widgets-agent:$(VERSION) .
