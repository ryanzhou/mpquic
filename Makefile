VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test race bench docker dist clean
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/mpquic ./cmd/mpquic
	CGO_ENABLED=0 go build -trimpath -o bin/mpbench ./cmd/mpbench
test:
	go test ./...
race:
	go test -race ./...
bench: build
	scripts/bench-matrix.sh
docker:
	docker build --build-arg VERSION=$(VERSION) -t mpquic:$(VERSION) -t mpquic:latest .
dist:
	for p in linux/amd64 linux/arm64 linux/arm darwin/arm64 windows/amd64; do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/mpquic-$$os-$$arch$$ext ./cmd/mpquic; \
	done
clean:
	rm -rf bin dist
