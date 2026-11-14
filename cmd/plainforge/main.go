// Command plainforge serves bare Git repositories over smart HTTP, announces
// pushes to Keelworks and mirrors source commits and tags outward.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/plainforge/internal/mirror"
	"github.com/ClearedStack/plainforge/internal/notify"
	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/server"
	"github.com/ClearedStack/plainforge/internal/store"
)

type config struct {
	listen          string
	repoDir         string
	reposDir        string
	initRepos       string
	notifyURL       string
	notifyTokenFile string
	mirrorURL       string
	mirrorTokenFile string
}

func parseFlags(args []string, getenv func(string) string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("plainforge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "address to listen on")
	fs.StringVar(&cfg.repoDir, "repo", "", "directory of a single bare repository")
	fs.StringVar(&cfg.reposDir, "repos", "", "root directory holding bare <name>.git repositories")
	fs.StringVar(&cfg.initRepos, "init-repos", "", "comma-separated repository names to create under -repos when missing")
	fs.StringVar(&cfg.notifyURL, "notify-url", getenv("PLAINFORGE_NOTIFY_URL"), "URL that receives a JSON POST for each announced push")
	fs.StringVar(&cfg.notifyTokenFile, "notify-token-file", getenv("PLAINFORGE_NOTIFY_TOKEN_FILE"), "file holding the bearer token for -notify-url")
	fs.StringVar(&cfg.mirrorURL, "mirror-url", getenv("PLAINFORGE_MIRROR_URL"), "base HTTPS URL for outbound Git push-mirroring")
	fs.StringVar(&cfg.mirrorTokenFile, "mirror-token-file", getenv("PLAINFORGE_MIRROR_TOKEN_FILE"), "file holding the token for -mirror-url")
	if err := fs.Parse(args); err != nil {
		return cfg, pferr.Parsef("usage: plainforge (-repos DIR | -repo DIR) [-init-repos LIST] [-listen ADDR] [-notify-url URL] [-notify-token-file FILE] [-mirror-url URL] [-mirror-token-file FILE]")
	}
	if fs.NArg() != 0 || (cfg.repoDir == "" && cfg.reposDir == "") || (cfg.repoDir != "" && cfg.reposDir != "") {
		return cfg, pferr.Parsef("usage: plainforge (-repos DIR | -repo DIR) [-init-repos LIST] [-listen ADDR] [-notify-url URL] [-notify-token-file FILE] [-mirror-url URL] [-mirror-token-file FILE]")
	}
	if cfg.initRepos != "" && cfg.reposDir == "" {
		return cfg, pferr.Parsef("-init-repos requires -repos")
	}
	return cfg, nil
}

func buildServer(ctx context.Context, cfg config, logger *log.Logger) (*server.Server, string, error) {
	srv := &server.Server{Log: logger}
	var notifySpoolDir string
	var repos []*store.Repo
	var targetDesc string

	if cfg.reposDir != "" {
		var initNames []string
		if cfg.initRepos != "" {
			initNames = strings.Split(cfg.initRepos, ",")
		}
		cat, err := store.OpenCatalog(cfg.reposDir, initNames)
		if err != nil {
			return nil, "", pferr.Public(err, "the repository catalog could not be opened")
		}
		srv.Catalog = cat
		repos = cat.Repos()
		notifySpoolDir = filepath.Join(cat.SpoolDir(), "notify")
		targetDesc = "catalog"
	} else {
		repo, err := store.Open(cfg.repoDir)
		if err != nil {
			return nil, "", pferr.Public(err, "the repository could not be opened")
		}
		srv.Repo = repo
		repos = []*store.Repo{repo}
		notifySpoolDir = filepath.Join(repo.SpoolDir(), "notify")
		targetDesc = "repository " + repo.Name()
	}

	if cfg.notifyURL != "" {
		sender := notify.NewHTTP(cfg.notifyURL)
		sender.TokenFile = cfg.notifyTokenFile
		sp, err := notify.OpenSpool(notifySpoolDir, sender, logger)
		if err != nil {
			return nil, "", pferr.Public(err, "the notification spool could not be opened")
		}
		go sp.Run(ctx, 10*time.Second)
		srv.Notify = sp
	}

	if cfg.mirrorURL != "" {
		pusher := mirror.NewHTTP(cfg.mirrorURL)
		pusher.TokenFile = cfg.mirrorTokenFile
		mq := mirror.NewQueue(pusher, logger)
		mq.Recover(repos)
		go mq.Run(ctx, 15*time.Second)
		srv.Mirror = mq
	}

	return srv, targetDesc, nil
}

func main() {
	logger := log.New(os.Stderr, "plainforge: ", 0)
	cfg, err := parseFlags(os.Args[1:], os.Getenv)
	if err != nil {
		logger.Print(err)
		os.Exit(2)
	}
	// Keep the Go heap well inside the 64 MiB budget unless told otherwise.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(48 << 20)
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	srv, targetDesc, err := buildServer(bgCtx, cfg, logger)
	if err != nil {
		logger.Print(err)
		os.Exit(1)
	}

	hs := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(discard{}, "", 0),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		bgCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()

	onOff := map[bool]string{true: "on", false: "off"}
	logger.Printf("serving %s, version %s, notifications %s, mirror %s",
		targetDesc, server.Version, onOff[srv.Notify != nil], onOff[srv.Mirror != nil])
	if err := hs.ListenAndServe(); err != http.ErrServerClosed {
		logger.Print(pferr.Parsef("the listen address could not be used"))
		os.Exit(1)
	}
}

// discard drops the HTTP server's own log lines, which can name peers.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
