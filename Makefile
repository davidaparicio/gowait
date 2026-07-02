.PHONY: build test vet run docker demo clean

build:
	go build -o bin/gowait ./cmd/gowait

test:
	go test ./... -race -count=1

vet:
	go vet ./...

run: build
	./bin/gowait -backend $${GOWAIT_BACKEND_URL:-http://localhost:9000}

docker:
	docker build -t gowait .

demo:
	docker compose up --build

demo-valkey:
	docker compose -f docker-compose.yml -f docker-compose.valkey.yml up --build

test-valkey:
	docker run -d --rm --name gowait-test-valkey -p 6390:6379 valkey/valkey:9.1.0-alpine
	go test ./internal/store/valkeystore/ -race -count=1 -v; docker stop gowait-test-valkey

clean:
	rm -rf bin
