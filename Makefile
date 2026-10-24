# リリースのワークフローも make dist を呼ぶ。

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

export CGO_ENABLED = 0
GOFLAGS_BUILD = -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

EXTRA = README.md deploy

.PHONY: build test dist clean

build:
	go build $(GOFLAGS_BUILD) -o mekiki ./cmd/mekiki

test:
	go test ./...

dist: test
	@set -eu; \
	rm -rf build dist; mkdir -p build dist; \
	for target in $(TARGETS); do \
	  os=$${target%/*}; arch=$${target#*/}; \
	  name=mekiki-$(VERSION)-$$os-$$arch; \
	  ext=; if [ "$$os" = windows ]; then ext=.exe; fi; \
	  mkdir -p build/$$name; \
	  GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o build/$$name/mekiki$$ext ./cmd/mekiki; \
	  cp -r $(EXTRA) build/$$name/; \
	  if [ "$$os" = windows ]; then \
	    (cd build && zip -qr ../dist/$$name.zip $$name); \
	  else \
	    tar -C build -czf dist/$$name.tar.gz $$name; \
	  fi; \
	done; \
	cd dist; if command -v sha256sum >/dev/null; then sha256sum * > SHA256SUMS; else shasum -a 256 * > SHA256SUMS; fi
	@ls -l dist

clean:
	rm -rf build dist mekiki
