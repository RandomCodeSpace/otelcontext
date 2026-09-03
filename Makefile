.PHONY: build test vet check setup-hooks loadtest loadtest-build release gate-tools gate-build gate-run

build:
	CGO_ENABLED=0 go build ./...

vet:
	go vet ./...

test:
	CGO_ENABLED=1 go test -race -timeout 120s ./...

## check runs the same steps as CI: build, vet, and test.
check: build vet test

loadtest-build:
	CGO_ENABLED=0 go build -tags loadtest -o bin/loadsim ./test/loadsim

loadtest: loadtest-build
	@echo "Running 200-service load simulator (60s) against localhost:4317..."
	./bin/loadsim

GATE_BIN_DIR ?= bin

## gate-tools builds the source-bound test tools without rebuilding the signed
## server candidate consumed by a certifying release run.
gate-tools:
	install -d "$(GATE_BIN_DIR)"
	CGO_ENABLED=0 go build -trimpath -tags loadtest -o "$(GATE_BIN_DIR)/loadsim" ./test/loadsim
	CGO_ENABLED=1 go build -trimpath -tags prefill -o "$(GATE_BIN_DIR)/aggprefill" ./test/aggprefill
	CGO_ENABLED=0 go build -trimpath -tags gate -o "$(GATE_BIN_DIR)/gate" ./test/gate

## gate-build builds every binary the historical diagnostic gate drives.
## The gate itself is behind the gate build tag, so ordinary Go commands
## ignore it.
gate-build: gate-tools
	CGO_ENABLED=1 go build -o "$(GATE_BIN_DIR)/otelcontext" .

## gate-run executes the manual seven-day gate protocol. It writes reports
## under docs/gates and exits non-zero unless every assertion passes.
gate-run: gate-build
	./bin/gate -config test/gate/gate.config.json -out docs/gates

## release verifies the Go build and the required status checks on main, then
## cuts an annotated tag once (RELEASE=--release also opens a draft GitHub
## release; RELEASE=--dry-run only reports). The release workflow builds, signs,
## proves and publishes the draft. The committed browser UI is embedded
## automatically by go:embed, including for go install.
release:
	./scripts/release.sh $(VERSION) $(RELEASE)

## setup-hooks installs the pre-commit hook into .git/hooks.
setup-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "✅ pre-commit hook installed"
