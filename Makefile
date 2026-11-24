# Price Tag - common tasks. Needs Go 1.24+; the browser tests also need Node 20+ and Playwright's Chromium.
GO      ?= go
BIN     := bin/pricetag
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
export GOTOOLCHAIN ?= local

.PHONY: build test test-go test-js test-e2e test-all vet fmt demo run loadtest docker clean

build:                      ## build the single server binary (web apps are embedded in it)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/pricetag

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w cmd internal tools web

test-go:                    ## Go unit + integration tests with the race detector
	$(GO) test -race -count=1 ./...

test-js: build              ## decoder, barcode and web-asset guard tests (no browser)
	cd tests && node --test js/*.test.js

test-e2e: build             ## real-browser tests: live updates, offline, camera, admin, slow phone
	cd tests && node --test --test-concurrency=1 e2e/*.e2e.js

test-all: vet test-go test-js test-e2e

# One command to try everything: seeds a demo shop and serves it.
# Open http://localhost:8080/s/demo/  (and /s/demo/admin/, /s/demo/test-sheet)
demo: build
	$(BIN) demo -data ./data
	$(BIN) serve -addr :8080 -data ./data

# Same, over HTTPS with a throw-away certificate, so a phone on your Wi-Fi may use its camera.
demo-phone: build
	$(BIN) demo -data ./data
	$(BIN) serve -addr :8443 -data ./data -dev-tls

run: build
	$(BIN) serve -addr :8080 -data ./data

loadtest: build
	$(GO) build -o bin/loadtest ./tools/loadtest
	@echo "see tools/loadtest/main.go header for how to run it"

docker:
	docker build -t pricetag:$(VERSION) --build-arg VERSION=$(VERSION) .

clean:
	rm -rf bin coverage.out
