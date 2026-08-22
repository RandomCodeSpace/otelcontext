.PHONY: build test vet check setup-hooks ui-install ui-build dev-ui loadtest loadtest-build release gate-build gate-run

ui-install:
	cd ui && npm install

ui-build:
	cd ui && npm run build
	touch internal/ui/dist/.gitkeep   # vite emptyOutDir wipes it; keep the source-only placeholder

build: ui-build
	CGO_ENABLED=0 go build ./...

vet:
	go vet ./...

test:
	CGO_ENABLED=1 go test -race -timeout 120s ./...

## check runs the same steps as CI: build → vet → test
check: build vet test

dev-ui:
	cd ui && npm run dev

loadtest-build:
	CGO_ENABLED=0 go build -tags loadtest -o bin/loadsim ./test/loadsim

loadtest: loadtest-build
	@echo "Running 200-service load simulator (60s) against localhost:4317..."
	./bin/loadsim

## gate-build builds every binary the seven-day release gate drives (#202).
## The gate itself is behind the `gate` build tag, so `go build ./...` and
## `go test ./...` ignore it.
gate-build:
	CGO_ENABLED=1 go build -o bin/otelcontext .
	CGO_ENABLED=0 go build -tags loadtest -o bin/loadsim ./test/loadsim
	CGO_ENABLED=1 go build -tags prefill -o bin/aggprefill ./test/aggprefill
	CGO_ENABLED=0 go build -tags gate -o bin/gate ./test/gate

## gate-run executes the manual seven-day gate protocol: roughly four to five
## hours of prefill, sustained churn, burst, kill -9 and measurement. It writes
## docs/gates/<date>-aggregate-7day-gate.{json,md} and exits non-zero unless
## every assertion passed. See docs/gates/README.md before running it.
gate-run: gate-build
	./bin/gate -config test/gate/gate.config.json -out docs/gates

## release builds the UI and cuts a tag whose tree embeds it, so
## `go install ...@<tag>` is UI-complete while main stays source-only.
## Usage: make release VERSION=vX.Y.Z [RELEASE=--release]
release:
	./scripts/release.sh $(VERSION) $(RELEASE)

## setup-hooks installs the pre-commit hook into .git/hooks
setup-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "✅ pre-commit hook installed"
