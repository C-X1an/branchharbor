SHELL := /bin/sh
.PHONY: bootstrap build run fmt fmt-check lint typecheck test unit integration acceptance security race fuzz demo verify clean

bootstrap:
	@test "$$(uname -s)" = Linux || { echo 'Use Linux or WSL2 with a Linux filesystem.'; exit 1; }
	@test "$$(go env GOVERSION)" = "go$$(cat .go-version)" || { echo 'Install the Go version pinned in .go-version.'; exit 1; }
	@python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 11) and __debug__ else "Python 3.11+ without optimization is required")'
build: bootstrap
	mkdir -p bin
	go build -trimpath -o bin/branchharbor ./cmd/branchharbor
run: build
	./bin/branchharbor serve --dir .bh
fmt:
	gofmt -w cmd internal tests
fmt-check:
	@files=$$(gofmt -l cmd internal tests) || exit 1; test -z "$$files" || { echo "$$files"; exit 1; }
lint:
	go vet ./...
typecheck: lint
test:
	go test -count=1 ./...
unit:
	go test -count=1 ./internal/...
integration:
	go test -count=1 ./internal/api ./internal/store -run 'Test(CrashRecovery|SingleWriter|AuthenticationHTTP|ScopeHTTP|AdminBoundary|BackupRestore)'
acceptance:
	go test -count=1 ./tests/acceptance_locked
security:
	go test -count=1 ./internal/auth ./internal/api ./internal/store -run 'Test(Authentication|Scope|AdminBoundary|Paths|SafeDefaults|Limits|Admission|UnsafeLayout|KeyPermissions)'
race:
	go test -race -count=1 ./...
fuzz:
	go test ./internal/store -run '^$$' -fuzz '^FuzzPath$$' -fuzztime=5s -parallel=2
	go test ./internal/store -run '^$$' -fuzz '^FuzzJournalFrame$$' -fuzztime=5s -parallel=2
	go test ./internal/store -run '^$$' -fuzz '^FuzzMergeTrees$$' -fuzztime=5s -parallel=2
	go test ./internal/strictjson -run '^$$' -fuzz '^FuzzStrictJSON$$' -fuzztime=5s -parallel=2
	go test ./internal/auth -run '^$$' -fuzz '^FuzzToken$$' -fuzztime=5s -parallel=2
demo: build
	python3 scripts/demo.py
verify: bootstrap fmt-check lint test race demo
clean:
	rm -f bin/branchharbor
