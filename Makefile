.PHONY: build test cover lint docker-build docker-run clean run \
	migrate-up migrate-down migrate-new

build:
	go build -o bin/server ./cmd/pricing-quote

test:
	go test ./cmd/... ./internal/... -race -coverprofile=coverage.out -coverpkg=./cmd/...,./internal/...

cover: test
	@go tool cover -func=coverage.out | tail -1
	@echo "Coverage report: coverage.out (html: go tool cover -html=coverage.out)"

lint:
	golangci-lint run

run:
	go run ./cmd/pricing-quote

migrate-up:
	go run ./cmd/migrate -direction up

migrate-down:
	go run ./cmd/migrate -direction down

migrate-new:
	@test -n "$(NAME)" || (echo "usage: make migrate-new NAME=add_widgets" && exit 1)
	@next=$$(printf '%03d' $$(( $$(ls internal/migrations/*.up.sql 2>/dev/null | wc -l | tr -d ' ') + 1 ))); \
	touch internal/migrations/$${next}_$(NAME).up.sql internal/migrations/$${next}_$(NAME).down.sql; \
	echo "created internal/migrations/$${next}_$(NAME).{up,down}.sql"

docker-build:
	docker build -t ai-crypto-onramp/pricing-quote .

docker-run:
	docker run --rm -p 8080:8080 ai-crypto-onramp/pricing-quote

clean:
	rm -rf bin/ coverage.out