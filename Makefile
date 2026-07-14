.PHONY: build test test-race cover lint docker docker-run clean run

build:
	go build -o bin/server ./cmd/pricing-quote

test:
	go test ./cmd/... ./internal/... -race -coverprofile=coverage.out -coverpkg=./cmd/...,./internal/...

test-race:
	go test -race ./cmd/... ./internal/...

cover: test
	@go tool cover -func=coverage.out | tail -1
	@echo "Coverage report: coverage.out (html: go tool cover -html=coverage.out)"

lint:
	golangci-lint run

run:
	go run ./cmd/pricing-quote

docker:
	docker build -t ai-crypto-onramp/pricing-quote .

docker-run:
	docker run --rm -p 8080:8080 ai-crypto-onramp/pricing-quote

clean:
	rm -rf bin/ coverage.out server pricing-quote