.PHONY: test build run

test:
	go test ./...

build:
	CGO_ENABLED=1 go build -trimpath -o bin/voxmail ./cmd/voxmail

run:
	VOXMAIL_ENCRYPTION_KEY=development-only-key-change-me-please-123456 ./bin/voxmail
