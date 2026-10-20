# mekiki

RSS/Atomフィードを集め、AIが注目記事を選んで日本語で要約した「今日のページ」を作るWebアプリ。
サーバで常駐させ、ブラウザから見る。

- **今日のページ（メイン）**: 設定した時刻（1日に複数回でもよい）に、前回の実行以降に増えた記事からAIが
  注目記事を選び、要約して今日のページに追記する。日付ごとにページが保存され、アーカイブから過去の日を辿れる。
  ページの先頭にはその日の「今日のまとめ」も付く。ボタンで手動実行・今日のページの作り直しもできる
- **プロファイル**: 「Tech」「ニュース」のように用途ごとにプロファイルを作り、画面上部で切り替える。
  フィード・選定基準・興味・実行時刻・ページ・Discord投稿先はプロファイルごとに別
- **RSSリーダー（サブ）**: 取得した記事をフィード・カテゴリ別に一覧できる。取得は定期実行＋「今すぐ取得」ボタン
- **AI連携**: 次から選べる（全プロファイル共通）
  - Anthropic API（デフォルト）
  - OpenAI / Google Gemini / OpenRouter
  - OpenAI互換API（URLを指定。Ollama・LM Studio・vLLMなどのローカルLLMも可）
  - `claude -p`（Claude Code CLI。サブスクリプション枠）
- **Discord投稿（任意）**: プロファイルにWebhook URLを設定すると、追加された記事をDiscordにも投稿する
- 設定・フィード管理はすべてWeb画面で行う。単一の静的リンクバイナリで、データはSQLiteファイル1つ

## セットアップ

### 1. ビルド

Go 1.26以上:

```sh
CGO_ENABLED=0 go build -o mekiki ./cmd/mekiki
```

またはNix:

```sh
nix build                 # → result/bin/mekiki（テストも実行される）
nix build .#with-claude   # claude -p 経路を使う場合。PATHにclaude-codeを含めたラッパー
```

### 2. 起動

```sh
MEKIKI_PASSWORD=好きなパスワード ./mekiki serve -addr :8080 -data ./data
```

`http://サーバ:8080/` を開き、ユーザー名は任意・パスワードは `MEKIKI_PASSWORD` でログインする。

| 環境変数 / フラグ | 内容 |
|---|---|
| `-addr` / `MEKIKI_ADDR` | 待ち受けアドレス（既定 `:8080`） |
| `-data` / `MEKIKI_DATA` | データディレクトリ（既定 `./data`）。`mekiki.db` が作られる |
| `MEKIKI_PASSWORD` | 設定するとBasic認証をかける。**外部に公開するなら必ず設定する** |
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` / `GEMINI_API_KEY` / `OPENROUTER_API_KEY` / `OPENAI_COMPAT_API_KEY` | 画面でAPIキーを入れない場合に使うキー |

Basic認証は通信を暗号化しないので、インターネット越しに使う場合はHTTPS化したリバースプロキシ
（Caddy・nginxなど）の内側に置く。

### 3. 画面での初期設定

初回起動時は「メイン」プロファイルが1つだけある。

1. **全体設定**（右上）— AI連携を選ぶ。「AI接続テスト」ボタンで疎通を確認できる
   - Anthropic API: APIキーを入力（または環境変数 `ANTHROPIC_API_KEY`）。モデルの既定は `claude-opus-5-5`
   - OpenAI / Gemini / OpenRouter: APIキーとモデル名を入力（OpenRouterのモデル名は `openai/gpt-5.1` の形式）
   - OpenAI互換: APIのURL（例 Ollamaなら `http://localhost:11434/v1`）とモデル名。キーが不要なら空のまま
   - claude -p: mekikiを動かすユーザーで、サーバ上で一度 `claude` を起動してログインしておく
2. **プロファイル**（上部の「＋」）— 必要ならプロファイルを追加する。表示名とID（URLに使う。例 `tech`）を決める。
   既存プロファイルの選定基準などをコピーして作ることもできる
3. **フィード** — プロファイルごとにフィードURLを追加するか、OPMLをインポートする（形式は `feeds/example.opml`）
4. **プロファイル設定** — 興味・趣向、選定基準、要約実行の時刻（例 `07:00, 12:00, 18:00`）、Discordの投稿先を入れる
5. **今日** — 「今すぐ要約実行」を押すと最初のページができる

AI連携が未設定のうちは、定時の要約実行は保留される（設定が済むと、その日の直近の枠が実行される）。

## 動き方

- **フィード取得**: 設定した間隔（既定60分）で全フィードを取得する。要約実行の直前にも取得する
- **要約実行（追記）**: 前回の実行で選定に渡していない新着記事だけを候補にし、今日のページに追記する。
  今日すでに載っている記事のタイトルも渡し、同じ話題を選ばないようにしている。新着がなければ何もしない
- **今日のページを作り直す**: 今日のページを消し、今日の候補記事すべてからページを作り直す
  （選定・要約が成功してから置き換えるので、失敗しても元のページは残る）
- 選定に失敗した実行は記事を消費しない（次回の実行で再び候補になる）
- 実行の結果とAIの使用量（トークン数）は「履歴」画面と各日のページ下部で確認できる
- 要約実行は同時に1つ。複数のプロファイルの実行時刻が重なった場合は順番に実行される
- 本文が取れない記事をAIにWebページを取得させて要約する救済は、Anthropic API と claude -p だけが対応する
  （他の経路ではタイトルから読み取れる範囲の要約になる）

## コマンド

```sh
mekiki serve                                     # Webサーバ＋スケジューラ
mekiki import-opml [-profile ID] FILE            # OPMLからフィードを登録
mekiki import-opml -profile news -new ニュース FILE # プロファイルを作ってから登録
mekiki fetch                                     # 全フィードを1回取得
mekiki digest [-profile ID] [-rebuild]           # 要約実行を1回（サーバ停止中のメンテナンス用）
```

`-profile` を省略すると最初のプロファイルが対象。いずれも `-data` でデータディレクトリを指定する。`fetch` / `digest` はサーバとは別プロセスで動くため、
サーバ稼働中はWeb画面のボタンを使う。

## systemdで常駐させる

`deploy/mekiki.service` を参考に:

```sh
sudo useradd -r -m -d /var/lib/mekiki -s /usr/sbin/nologin mekiki
sudo cp mekiki /usr/local/bin/
sudo cp deploy/mekiki.service /etc/systemd/system/
sudo systemctl edit mekiki      # MEKIKI_PASSWORD などを設定
sudo systemctl enable --now mekiki
```

`claude -p` 経路を使う場合は、サービスのユーザーで一度ログインしておく
（`sudo -u mekiki -H claude`。認証情報は `/var/lib/mekiki/.claude` に保存される）。

## 開発

```sh
nix develop      # go・gopls・claude-code
go test ./...
go run ./cmd/mekiki serve -data ./data
```

## ドキュメント

[SPEC.md](SPEC.md) — 仕様書。設計判断の経緯・運用で踏んだ障害の記録は付録A。
