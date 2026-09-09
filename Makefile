.PHONY: test build run

test:
	go vet ./...
	go test -race -count=1 ./...

build:
	CGO_ENABLED=1 go build -trimpath -o bin/voxmail ./cmd/voxmail

run:
	@test -n "$(VOXMAIL_ENCRYPTION_KEY)" || (echo 'set VOXMAIL_ENCRYPTION_KEY before running' >&2; exit 1)
	VOXMAIL_ENCRYPTION_KEY="$(VOXMAIL_ENCRYPTION_KEY)" ./bin/voxmail
