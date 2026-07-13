.PHONY: build test test-race cover lint vet docker docker-run clean run

build:
	go build -o bin/server .

test:
	go test ./... -race -coverprofile=coverage.out -coverpkg=./...

test-race:
	go test -race ./...

cover: test
	@go tool cover -func=coverage.out | tail -1
	@echo "Coverage report: coverage.out (html: go tool cover -html=coverage.out)"

lint: vet
	@echo "lint OK (go vet)"

vet:
	go vet ./...

run:
	go run .

docker:
	docker build -t ai-crypto-onramp/pricing-quote .

docker-run:
	docker run --rm -p 8080:8080 ai-crypto-onramp/pricing-quote

clean:
	rm -rf bin/ coverage.out server pricing-quote