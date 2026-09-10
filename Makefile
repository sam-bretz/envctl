.PHONY: build test vet lint install

build:
	go build -o bin/envctl ./cmd/envctl

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

install:
	go install ./cmd/envctl
