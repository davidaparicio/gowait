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

clean:
	rm -rf bin
