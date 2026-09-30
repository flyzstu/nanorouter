.PHONY: build run

BINARY := bin/nanorouter
CONFIG ?= config.yaml
# 版本号取 git tag/describe；无 git 环境（如容器内源码构建）回退 dev。
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	@mkdir -p $(dir $(BINARY))
	go build -buildvcs=false -trimpath -ldflags='-s -w -X main.version=$(VERSION)' -o $(BINARY) ./cmd/nanorouter

run:
	go run ./cmd/nanorouter --config $(CONFIG)
