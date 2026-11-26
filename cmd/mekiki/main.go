// mekiki: RSSフィードを集め、Claudeが注目記事を選んで要約した「今日のページ」を作るWebアプリ。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata" // tzdataのない最小環境でもタイムゾーン設定を使えるように

	"github.com/kienn-HCl/mekiki/internal/app"
	"github.com/kienn-HCl/mekiki/internal/conffile"
	"github.com/kienn-HCl/mekiki/internal/config"
	"github.com/kienn-HCl/mekiki/internal/digest"
	evalpkg "github.com/kienn-HCl/mekiki/internal/eval"
	"github.com/kienn-HCl/mekiki/internal/feeds"
	"github.com/kienn-HCl/mekiki/internal/store"
	"github.com/kienn-HCl/mekiki/internal/web"
)

var version = "dev"

const usage = `mekiki — RSSの注目記事をAIが選んで要約するWebアプリ

使い方:
  mekiki serve [-addr :8080] [-data ./data]   Webサーバとスケジューラを起動
  mekiki import-opml [-profile ID] FILE       OPMLからフィードを登録（-new でプロファイルも作成）
  mekiki fetch [-data ./data]                 全フィードを1回取得
  mekiki digest [-profile ID] [-rebuild]      要約実行を1回行う（今日のページに追記）
  mekiki eval select [オプション]             保存済みの候補で選定をやり直し、本番と比べる
  mekiki eval summarize [オプション]          保存済みの本文で要約をやり直し、本番と並べる
                                              （オプションは mekiki eval select -h。-run を付けるまでAIには送らない）

  -profile を省略すると最初のプロファイルが対象。各コマンドとも -data でデータディレクトリを指定
  mekiki version

環境変数:
  MEKIKI_ADDR       待ち受けアドレス（-addr の既定値。既定 :8080）
  MEKIKI_DATA       データディレクトリ（-data の既定値。既定 ./data）
  MEKIKI_CONFIG     設定ディレクトリ（-config の既定値。config.toml と feeds/*.opml を置く。既定はデータディレクトリ）
  MEKIKI_PASSWORD   設定するとWeb画面にBasic認証をかける
  MEKIKI_USER       Basic認証のユーザー名（既定 mekiki）
  ANTHROPIC_API_KEY / OPENAI_API_KEY / GEMINI_API_KEY / OPENROUTER_API_KEY
                    画面でAPIキーを設定していない場合に使うキー
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
	case "eval":
		err = evalCmd(args)
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

// openStore はDBを開き、設定ファイル（config.toml）の内容をDBに反映する。
func openStore(fs *flag.FlagSet, args []string) (*store.Store, *conffile.Manager, error) {
	data := fs.String("data", env("MEKIKI_DATA", "./data"), "データディレクトリ")
	cfgDir := fs.String("config", env("MEKIKI_CONFIG", ""), "設定ディレクトリ（config.toml と feeds/。既定はデータディレクトリ）")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(*data)
	if err != nil {
		return nil, nil, err
	}
	if *cfgDir == "" {
		*cfgDir = *data
	}
	conf := conffile.New(st, *cfgDir)
	if err := conf.Init(context.Background()); err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, conf, nil
}

// findProfile は -profile で指定されたプロファイル（空なら最初のもの）を返す。
func findProfile(st *store.Store, slug string) (config.Profile, error) {
	ctx := context.Background()
	if slug != "" {
		p, err := st.ProfileBySlug(ctx, slug)
		if errors.Is(err, store.ErrNotFound) {
			return p, fmt.Errorf("プロファイル %q がありません", slug)
		}
		return p, err
	}
	if err := st.EnsureProfile(ctx); err != nil {
		return config.Profile{}, err
	}
	list, err := st.Profiles(ctx)
	if err != nil {
		return config.Profile{}, err
	}
	return list[0], nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("MEKIKI_ADDR", ":8080"), "待ち受けアドレス")
	st, conf, err := openStore(fs, args)
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
	a.Conf = conf
	slog.Info("設定ファイル", "path", conf.Path())
	srv, err := web.New(a, env("MEKIKI_USER", "mekiki"), os.Getenv("MEKIKI_PASSWORD"))
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
	slug := fs.String("profile", "", "登録先のプロファイルID")
	newName := fs.String("new", "", "このプロファイル名で -profile のプロファイルを新規作成してから登録")
	st, conf, err := openStore(fs, args)
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
	if *newName != "" {
		p := config.DefaultProfile(*slug, *newName)
		if err := p.Validate(); err != nil {
			return err
		}
		if _, err := st.CreateProfile(context.Background(), p); err != nil {
			return err
		}
	}
	p, err := findProfile(st, *slug)
	if err != nil {
		return err
	}
	added := 0
	for _, fd := range list {
		ok, err := st.AddFeed(context.Background(), p.ID, fd.Title, fd.URL, fd.Category)
		if err != nil {
			return err
		}
		if ok {
			added++
		}
	}
	fmt.Printf("プロファイル「%s」に%d件を追加しました（%d件は登録済み）\n", p.Name, added, len(list)-added)
	return conf.Save(context.Background())
}

func fetchOnce(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	st, _, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := app.New(st).FetchNow(context.Background(), 0)
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
	slug := fs.String("profile", "", "対象のプロファイルID")
	st, _, err := openStore(fs, args)
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := findProfile(st, *slug)
	if err != nil {
		return err
	}
	kind := digest.KindAppend
	if *rebuild {
		kind = digest.KindRebuild
	}
	return app.New(st).Digest.Run(context.Background(), digest.Options{ProfileID: p.ID, Kind: kind, Trigger: digest.TriggerManual})
}

func evalCmd(args []string) error {
	if len(args) == 0 || (args[0] != "select" && args[0] != "summarize") {
		return errors.New("使い方: mekiki eval select|summarize [オプション]")
	}
	what := args[0]
	fs := flag.NewFlagSet("eval "+what, flag.ExitOnError)
	slug := fs.String("profile", "", "対象のプロファイルID")
	days := fs.Int("days", 14, "何日前までの記録を使うか")
	limit := fs.Int("limit", 0, "select: 実行の数 / summarize: 記事の数の上限（0なら無制限、summarizeの既定は20）")
	backend := fs.String("backend", "", "AI連携方式を上書き（anthropic / openai / gemini / openrouter / compat / cli）")
	model := fs.String("model", "", "モデルを上書き")
	effort := fs.String("effort", "", "effortを上書き（low / medium / high など。none で送らない）")
	pick := fs.Int("pick", 0, "select: 選ぶ件数（0ならプロファイルの値）")
	maxChars := fs.Int("max-chars", 0, "summarize: 要約に渡す本文の上限文字数（0なら記録された全文）")
	judge := fs.String("judge", "", "summarize: 要約の忠実性を採点させるモデル（同じ経路で呼ぶ）")
	out := fs.String("out", "", "summarize: 本番とやり直しの要約を並べたレポートの出力先（既定はデータディレクトリの eval/）")
	run := fs.Bool("run", false, "実際にAIへ問い合わせる（付けなければ問い合わせ内容と量の表示だけ）")
	st, _, err := openStore(fs, args[1:])
	if err != nil {
		return err
	}
	defer st.Close()
	p, err := findProfile(st, *slug)
	if err != nil {
		return err
	}
	ctx := context.Background()
	kv, err := st.Settings(ctx)
	if err != nil {
		return err
	}
	cfg := config.Load(kv)
	if *backend != "" {
		cfg.Backend = *backend
	}
	if *model != "" {
		if cfg.Backend == config.BackendCLI {
			cfg.CLIModel = *model
		} else {
			cfg.Models[cfg.Backend] = *model
		}
	}
	if *effort == "none" {
		cfg.Efforts[cfg.Backend] = ""
	} else if *effort != "" {
		cfg.Efforts[cfg.Backend] = *effort
	}
	opt := evalpkg.Options{
		Profile: p, Settings: cfg, Since: time.Now().AddDate(0, 0, -*days), Limit: *limit, Pick: *pick,
		MaxChars: *maxChars, Judge: *judge, Run: *run, Out: os.Stdout,
	}
	r := &evalpkg.Runner{Store: st, NewBackend: app.NewBackend}
	if what == "select" {
		_, err = r.Select(ctx, opt)
		return err
	}
	if opt.Limit == 0 {
		opt.Limit = 20
	}
	if !opt.Run {
		_, err = r.Summarize(ctx, opt, io.Discard)
		return err
	}
	path := *out
	if path == "" {
		dir := filepath.Join(fs.Lookup("data").Value.String(), "eval")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		path = filepath.Join(dir, "summarize-"+time.Now().Format("20060102-150405")+".md")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = r.Summarize(ctx, opt, f)
	fmt.Println("レポート:", path)
	return err
}
