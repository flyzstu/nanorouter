.PHONY: build run

BINARY := bin/nanorouter
CONFIG ?= config.yaml

build:
	@mkdir -p $(dir $(BINARY))
	go build -buildvcs=false -o $(BINARY) ./cmd/nanorouter

run:
	go run ./cmd/nanorouter -config $(CONFIG)
