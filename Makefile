# ビルド・テスト・配布物作成。GitHub Actions（.github/workflows/release.yml）もここを呼ぶので、
# Actionsで失敗したときは手元で同じ make を実行すれば再現できる。
#
#   make                 手元のOS向けに ./mekiki をビルド
#   make test            テスト
#   make dist            全ターゲット向けのアーカイブと SHA256SUMS を dist/ に作る
#   make dist TARGETS=linux/amd64   一部だけ
#   make clean

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TARGETS ?= linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# 静的リンクの単一バイナリ（SQLiteも純Go実装なのでCGO不要）
export CGO_ENABLED = 0
GOFLAGS_BUILD = -trimpath -ldflags "-s -w -X main.version=$(VERSION)"

# アーカイブに同梱するもの（feeds/ は手元の個人用OPMLを混ぜないよう例だけ）
EXTRA = README.md env.example deploy

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
	  echo "build $$name"; \
	  mkdir -p build/$$name; \
	  GOOS=$$os GOARCH=$$arch go build $(GOFLAGS_BUILD) -o build/$$name/mekiki$$ext ./cmd/mekiki; \
	  cp -r $(EXTRA) build/$$name/; \
	  mkdir -p build/$$name/feeds && cp feeds/example.opml build/$$name/feeds/; \
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
