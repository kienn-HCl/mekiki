// mekiki: RSSフィードを集め、Claudeが注目記事を選んで要約した「今日のページ」を作るWebアプリ。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // tzdataのない最小環境でもタイムゾーン設定を使えるように

	"github.com/kienn-HCl/mekiki/internal/app"
	"github.com/kienn-HCl/mekiki/internal/digest"
	"github.com/kienn-HCl/mekiki/internal/feeds"
	"github.com/kienn-HCl/mekiki/internal/store"
	"github.com/kienn-HCl/mekiki/internal/web"
)

var version = "dev"

const usage = `mekiki — RSSをAIが目利きして要約するWebアプリ

使い方:
  mekiki serve [-addr :8080] [-data ./data]   Webサーバとスケジューラを起動
  mekiki import-opml [-data ./data] FILE      OPMLからフィードを登録
  mekiki fetch [-data ./data]                 フィードを1回取得
  mekiki digest [-data ./data] [-rebuild]     要約実行を1回行う（今日のページに追記）
  mekiki version

環境変数:
  MEKIKI_ADDR       待ち受けアドレス（-addr の既定値。既定 :8080）
  MEKIKI_DATA       データディレクトリ（-data の既定値。既定 ./data）
  MEKIKI_PASSWORD   設定するとWeb画面にBasic認証をかける（ユーザー名は任意）
  ANTHROPIC_API_KEY 画面でAPIキーを設定していない場合に使うキー
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(args)
	case "import-opml":
		err = importOPML(args)
	case "fetch":
		err = fetchOnce(args)
	case "digest":
		err = digestOnce(args)
	case "version":
		fmt.Println("mekiki", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "不明なコマンド: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openStore(fs *flag.FlagSet, args []string) (*store.Store, error) {
	data := fs.String("data", env("MEKIKI_DATA", "./data"), "データディレクトリ")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return store.Open(*data)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("MEKIKI_ADDR", ":8080"), "待ち受けアドレス")
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := st.FailStaleRuns(ctx, time.Now()); err != nil {
		return err
	}

	a := app.New(st)
	srv, err := web.New(a, os.Getenv("MEKIKI_PASSWORD"))
	if err != nil {
		return err
	}
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go a.RunScheduler(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	if os.Getenv("MEKIKI_PASSWORD") == "" {
		slog.Warn("MEKIKI_PASSWORD が未設定のため、Web画面は認証なしで公開されます")
	}
	slog.Info("mekiki 起動", "version", version, "addr", *addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func importOPML(args []string) error {
	fs := flag.NewFlagSet("import-opml", flag.ExitOnError)
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	if fs.NArg() != 1 {
		return errors.New("OPMLファイルを1つ指定してください")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	list, err := feeds.ParseOPML(f)
	if err != nil {
		return err
	}
	added := 0
	for _, fd := range list {
		ok, err := st.AddFeed(context.Background(), fd.Title, fd.URL, fd.Category)
		if err != nil {
			return err
		}
		if ok {
			added++
		}
	}
	fmt.Printf("%d件を追加しました（%d件は登録済み）\n", added, len(list)-added)
	return nil
}

func fetchOnce(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := app.New(st).FetchNow(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("フィード%d件を取得（失敗%d件・スキップ中%d件）、新着記事%d件\n",
		res.Feeds, res.Failed, len(res.Skipped), res.Added)
	return nil
}

func digestOnce(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ExitOnError)
	rebuild := fs.Bool("rebuild", false, "今日のページを作り直す")
	st, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	kind := digest.KindAppend
	if *rebuild {
		kind = digest.KindRebuild
	}
	return app.New(st).Digest.Run(context.Background(), digest.Options{Kind: kind, Trigger: digest.TriggerManual})
}
