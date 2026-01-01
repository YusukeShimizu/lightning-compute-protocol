.PHONY: test lint fmt

test:
	go test ./...

lint:
	golangci-lint run $(shell go list -f '{{.Dir}}' ./...)

fmt:
	golangci-lint fmt $(shell go list -f '{{.Dir}}' ./...)
