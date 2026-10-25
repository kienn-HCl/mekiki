#!/bin/sh
# バイナリに組み込まれる依存モジュール（とGo標準ライブラリ）のライセンス文を標準出力にまとめる。
# 依存はOSによって変わるので、GOOS/GOARCH を指定して呼ぶ。
set -eu

pkg=${1:-./cmd/mekiki}
main=$(go list -m)

emit() { # 名前 ディレクトリ
	found=
	for f in "$2"/LICENSE* "$2"/LICENCE* "$2"/COPYING* "$2"/NOTICE*; do
		[ -f "$f" ] || continue
		found=1
		printf '================================================================\n%s (%s)\n\n' "$1" "${f##*/}"
		cat "$f"
		printf '\n'
	done
	if [ -z "$found" ]; then
		echo "ライセンスファイルが見つからない: $1 ($2)" >&2
		exit 1
	fi
}

emit "Go (標準ライブラリ・ランタイム)" "$(go env GOROOT)"
go list -deps -f '{{with .Module}}{{.Path}}@{{.Version}} {{.Dir}}{{end}}' "$pkg" |
	sort -u | grep -v "^$main@" |
	while read -r mod dir; do emit "$mod" "$dir"; done
