.PHONY: build test vet check setup-hooks loadtest loadtest-build release gate-build gate-run

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

## gate-build builds every binary the seven-day release gate drives.
## The gate itself is behind the gate build tag, so ordinary Go commands
## ignore it.
gate-build:
	CGO_ENABLED=1 go build -o bin/otelcontext .
	CGO_ENABLED=0 go build -tags loadtest -o bin/loadsim ./test/loadsim
	CGO_ENABLED=1 go build -tags prefill -o bin/aggprefill ./test/aggprefill
	CGO_ENABLED=0 go build -tags gate -o bin/gate ./test/gate

## gate-run executes the manual seven-day gate protocol. It writes reports
## under docs/gates and exits non-zero unless every assertion passes.
gate-run: gate-build
	./bin/gate -config test/gate/gate.config.json -out docs/gates

## release verifies the Go build and cuts a tag. The committed browser UI is
## embedded automatically by go:embed, including for go install.
release:
	./scripts/release.sh $(VERSION) $(RELEASE)

## setup-hooks installs the pre-commit hook into .git/hooks.
setup-hooks:
	cp scripts/pre-commit .git/hooks/pre-commit
	chmod +x .git/hooks/pre-commit
	@echo "✅ pre-commit hook installed"
