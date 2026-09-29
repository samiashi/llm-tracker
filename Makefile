GO       = go
BIN      = bin
# := rather than ?=, so a VERSION exported by a shell cannot stamp a dirty
# build as a release. `make VERSION=v1.4.0 ...` still overrides.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)
GOPKGS   = ./schema/... ./launchd/... ./agent/... ./server/...

# Serial, always: vite empties server/internal/web/dist before writing it, so
# a server build running beside it under -j embeds a half-written dashboard
# that 404s, with no error at build time.
.NOTPARALLEL:

.PHONY: all build agent server dashboard prices test test-go test-dashboard lint lint-go lint-dashboard fmt clean run-server tools

all: build

## build: dashboard first, so the server embeds the current frontend
build: dashboard agent server

# vite's emptyOutDir also deletes the tracked placeholder //go:embed needs in a
# fresh clone; restoring it keeps the tree clean.
dashboard:
	cd dashboard && npm ci --silent && npm run build
	@git checkout -- server/internal/web/dist/.gitkeep 2>/dev/null || \
		printf 'placeholder so //go:embed all:dist matches in a fresh clone\n' \
			> server/internal/web/dist/.gitkeep

agent:
	cd agent && CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o ../$(BIN)/llm-tracker-agent .

server:
	cd server && CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o ../$(BIN)/llm-tracker-server .

## prices: regenerate the embedded price table from LiteLLM's database
prices:
	python3 schema/scripts/gen_prices.py

## test: Go and dashboard suites, each under two timezones
##
## Santiago's DST transition is at midnight, the case naive date arithmetic
## gets wrong. -count=1 on both, because Go's test cache ignores TZ: either run
## could replay a result cached under another zone.
test: test-go test-dashboard

# CI runs these halves as separate jobs.
test-go:
	TZ=UTC $(GO) test -count=1 $(GOPKGS)
	TZ=America/Santiago $(GO) test -count=1 $(GOPKGS)
	python3 -m unittest discover -s schema/scripts

test-dashboard:
	cd dashboard && npm test --silent

## lint: static analysis for both halves
lint: lint-go lint-dashboard

lint-go:
	golangci-lint run $(GOPKGS)

lint-dashboard:
	cd dashboard && npm run lint && npm run format:check && npx tsc -b

## fmt: apply formatters in place
fmt:
	golangci-lint fmt $(GOPKGS)
	cd dashboard && npm run format && npm run lint:fix

## tools: check golangci-lint is installed, and install the dashboard's npm dependencies
tools:
	@command -v golangci-lint >/dev/null || { echo "install golangci-lint: brew install golangci-lint"; exit 1; }
	cd dashboard && npm ci --silent

# The dashboard at http://127.0.0.1:8790, with the database in the checkout.
run-server:
	cd server && $(GO) run . -v -db ../llm-tracker.db

# Keeps dist/.gitkeep, which //go:embed needs to compile. -exec rm, not
# -delete: -delete implies -depth on BSD find, and the ! -name guard then fails
# to protect the placeholder.
clean:
	rm -rf $(BIN) dashboard/node_modules
	find server/internal/web/dist -mindepth 1 -maxdepth 1 \
		! -name .gitkeep -exec rm -rf {} + 2>/dev/null || true
