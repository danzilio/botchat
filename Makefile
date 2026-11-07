VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e lint cover run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/botchat ./cmd/botchat

test:
	go test -race ./...

e2e:
	cd e2e && npm ci && npx playwright install chromium && npx playwright test

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

run: build
	./bin/botchat

clean:
	rm -rf bin dist coverage.out e2e/.bin e2e/test-results e2e/playwright-report
