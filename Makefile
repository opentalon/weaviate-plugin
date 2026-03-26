BINARY  = weaviate-plugin
PREFIX ?= /usr/local/bin

.PHONY: build install test test-unit lint clean

## build: compile the plugin binary for the current platform
build:
	go build -o $(BINARY) .

## install: build and copy the binary to PREFIX (default /usr/local/bin)
install: build
	install -m 755 $(BINARY) $(PREFIX)/$(BINARY)

## test-unit: run unit tests (no Weaviate required)
test-unit:
	go test -race -v ./...

## test: run full integration suite (requires Weaviate — see README)
test:
	go test -v -tags integration -timeout 120s ./...

## test-brew: integration tests using a local brew-installed Weaviate (keyword tests only)
test-brew:
	AUTHENTICATION_ANONYMOUS_ACCESS_ENABLED=true \
	DEFAULT_VECTORIZER_MODULE=none \
	PERSISTENCE_DATA_PATH="$(TMPDIR)/weaviate-plugin-test" \
	weaviate --host 0.0.0.0 --port 8080 --scheme http & \
	sleep 3 && \
	go test -v -tags integration -timeout 60s ./... ; \
	pkill -f "weaviate --host" || true

## test-docker: integration tests with full contextionary (nearText enabled)
test-docker:
	docker compose up -d
	until curl -sf http://localhost:8080/v1/.well-known/ready; do sleep 1; done
	WEAVIATE_MODULE=text2vec-contextionary \
	go test -v -tags integration -timeout 120s ./...
	docker compose down

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## clean: remove build artifacts
clean:
	rm -f $(BINARY) $(BINARY)-*

## build-linux: cross-compile for Linux amd64 (for k8s deployment)
build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY)-linux-amd64 .
