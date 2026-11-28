# DBの版を上げる手順

mekiki はデータベース（`mekiki.db`）の版を `PRAGMA user_version` で持ち、知らない版のDBでは起動しない。
本体には古い版を読むための移行コードを入れないので、版が上がったときはここの手順でDBを更新してから新しい版を起動する。

どの手順も、mekiki を止めてから行い、先にDBを複製しておく。

```sh
sudo systemctl stop mekiki
cp /var/lib/mekiki/mekiki.db /var/lib/mekiki/mekiki.db.bak
```

`sqlite3` コマンドがなければ、同じSQLを Python で流してもよい:
`python3 -c 'import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.executescript(sys.argv[2]); c.close()' mekiki.db "<SQL>"`

## 版2 → 版3

フィードに、載っている記事のうち最新の投稿日時（`last_item_at`）を持たせる。

```sh
sqlite3 /var/lib/mekiki/mekiki.db "ALTER TABLE feeds ADD COLUMN last_item_at INTEGER; PRAGMA user_version = 3;"
```

更新後の最初の取得では、最新の投稿日時をまだ知らないので全フィードを中身まで取得する（2回目からは今までどおり、変更がなければ中身を取らない）。
