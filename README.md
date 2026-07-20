# mekiki

RSS/Atomフィードから注目記事をClaudeに選ばせて、日本語の要約付きでDiscordに投稿するbot。
cronでの定期実行を想定している。

1回の実行の流れ:

1. OPMLに登録したフィードから最近の記事を集める
2. 記事タイトルの一覧をClaude（Claude Code CLI）に渡し、注目記事を選ばせる
3. 選ばれた記事ごとに本文を取得し、日本語2〜3文の要約を生成する
4. タイトル・リンク・要約・記事画像をembedにしてDiscord Webhookへ投稿する

1つのコードベースを「プロファイル」（TOML）で切り替えて、用途の異なる複数のbot（技術記事用・ニュース用など）として動かせる。

## 必要なもの

- Nix（flakes有効）。Python・feedparser・claude-code はflakeが提供する
- Claude Code CLIのログイン済み認証（サブスクリプション枠で動く。APIキー不要）
- 投稿先DiscordチャンネルのWebhook URL

## 用意するファイル

以下はリポジトリに含まれない（個人データのためgit管理外）。実行前に自分で用意する:

| ファイル | 内容 |
|---|---|
| `feeds/<名前>.opml` | 購読フィード一覧。`feeds/example.opml` の形式（カテゴリ→フィードの2階層） |
| `profiles/<名前>.toml` | プロファイル。`profiles/rss.toml` を雛形に編集 |
| `user_memory.txt`（任意） | 選定基準に加味したい興味・趣向の自由テキスト |
| `env`（cron運用時） | 環境変数ファイル。`env.example` をコピーして作成 |

プロファイルの最小構成:

```toml
name = "rss"
opml_path = "feeds/rss.opml"
webhook_env = "RSS_DISCORD_WEBHOOK_URL"   # Webhook URLを読む環境変数の名前

[prompts]
select_criteria = """
- 技術的に興味深いもの、実務に役立つものを優先してください
"""
```

## 動かす

```sh
nix develop
export RSS_DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/..."
python3 -m mekiki profiles/rss.toml
```

Webhookの環境変数が未設定の場合はドライラン（Discordへ送らず標準出力に表示）になる。

## ビルドとcron登録

```sh
nix build                                  # → result/bin/mekiki
cp env.example env && $EDITOR env && chmod 600 env
```

`result/bin/mekiki` は `MEKIKI_DIR` が設定されていればそこへcdしてから実行する
（cronは任意のカレントディレクトリで起動するため、envで指定しておく）。
プロファイル・OPML・キャッシュの相対パスはそこを基準に解決される。

```cron
0 8 * * * . /path/to/mekiki/env && /path/to/mekiki/result/bin/mekiki profiles/rss.toml >> /var/log/mekiki-rss.log 2>&1
```

実行失敗時はDiscordに⚠️通知が送られる（Discord自体に届かない障害のときはcronログが頼り）。

## テスト

```sh
nix develop -c python3 -m pytest tests/ -q         # 純関数の自動テスト（nix flake checkでも実行）
python3 test_parts.py profiles/rss.toml opml       # OPMLパース確認
python3 test_parts.py profiles/rss.toml summarize  # 要約・画像抽出の実地確認
```

## ドキュメント

[SPEC.md](SPEC.md) — 仕様書。設計判断の経緯・運用で踏んだ障害の記録は付録A。
