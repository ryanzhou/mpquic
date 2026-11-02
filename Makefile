VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# Windows file/product version: the numeric part of a vX.Y.Z tag, else 0.0.0.
WINVER = $(shell v=$$(echo "$(VERSION)" | sed -E 's/^v//; s/[^0-9.].*$$//'); echo "$${v:-0.0.0}" | grep -Eq '^[0-9]+(\.[0-9]+){0,3}$$' && echo "$${v:-0.0.0}" || echo 0.0.0)
GOWINRES := go run github.com/tc-hib/go-winres@v0.3.3

.PHONY: build test race bench docker dist winres clean
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
	for p in linux/amd64 linux/arm64 linux/arm darwin/arm64 windows/amd64 windows/arm64; do \
	  os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/mpquic-$$os-$$arch$$ext ./cmd/mpquic; \
	done
# Windows resources (manifest, icon, version info) linked into mpquic.exe.
# The generated .syso files are committed; release builds regenerate them
# with the tag's version (make winres VERSION=v1.2.3).
winres:
	cd cmd/mpquic && $(GOWINRES) make --in winres/winres.json --out rsrc --arch amd64,arm64 \
	  --product-version $(WINVER) --file-version $(WINVER)
clean:
	rm -rf bin dist
