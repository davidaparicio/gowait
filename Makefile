.PHONY: build test vet lint run docker demo demo-valkey demo-multi test-valkey loadtest release-snapshot clean

build:
	go build -o bin/gowait ./cmd/gowait

test:
	go test ./... -race -count=1

vet:
	go vet ./...

lint:
	golangci-lint run

release-snapshot:
	goreleaser release --snapshot --clean

run: build
	./bin/gowait -backend $${GOWAIT_BACKEND_URL:-http://localhost:9000}

docker:
	docker build -t gowait .

demo:
	docker compose up --build

demo-valkey:
	docker compose -f docker-compose.yml -f docker-compose.valkey.yml up --build

demo-multi:
	docker compose -f docker-compose.yml -f docker-compose.valkey.yml -f docker-compose.multi.yml up --build

test-valkey:
	docker run -d --rm --name gowait-test-valkey -p 6390:6379 valkey/valkey:9.1.0-alpine
	go test ./internal/store/valkeystore/ -race -count=1 -v; docker stop gowait-test-valkey

# Needs k6 (https://k6.io) and a gowait tuned for fast slot turnover, e.g.:
#   ./bin/gowait -backend http://localhost:9001 -capacity 200 \
#     -inactivity-ttl 6s -queue-ttl 8s -poll-interval 2s
loadtest:
	k6 run loadtest/waitingroom.js

clean:
	rm -rf bin
