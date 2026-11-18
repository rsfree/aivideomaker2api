GO       ?= go
GOFMT    ?= $(shell $(GO) env GOROOT)/bin/gofmt
BIN      := bin/aivideomaker2api
PROBE    := bin/avm2api-probe
IMAGE    ?= ghcr.io/rsfree/aivideomaker2api
TAG      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build probe test vet fmt run probe-run docker release clean

all: fmt vet test build probe

build:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/server

probe:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o $(PROBE) ./cmd/probe

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	$(GOFMT) -w .

run:
	$(GO) run ./cmd/server -config config.json

# 零成本自检：只读探测（余额 / 额度 / 闸门），不创建任务。
probe-run:
	$(GO) run ./cmd/probe -config config.json

docker:
	docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

release: vet test
	@echo "发版走 tag：git tag vX.Y.Z && git push --tags（.github/workflows/release.yml 会推 GHCR 多架构镜像）"

clean:
	@rm -rf bin
