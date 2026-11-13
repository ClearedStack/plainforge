// Command plainforge serves one bare Git repository over smart HTTP and
// announces pushes.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/clearedstack/plainforge/internal/notify"
	"github.com/clearedstack/plainforge/internal/pferr"
	"github.com/clearedstack/plainforge/internal/server"
	"github.com/clearedstack/plainforge/internal/store"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "address to listen on")
	repoDir := flag.String("repo", "", "directory of the bare repository, for example /data/example.git (created when missing)")
	notifyURL := flag.String("notify-url", "", "URL that receives a JSON POST for each announced push (empty: no announcements)")
	flag.Parse()

	logger := log.New(os.Stderr, "plainforge: ", 0)
	if *repoDir == "" || flag.NArg() != 0 {
		logger.Print(pferr.Parsef("usage: plainforge -repo DIR [-listen ADDR] [-notify-url URL]"))
		os.Exit(2)
	}
	// Keep the Go heap well inside the 64 MiB budget unless told otherwise.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(40 << 20)
	}

	repo, err := store.Open(*repoDir)
	if err != nil {
		logger.Print(pferr.Public(err, "the repository could not be opened"))
		os.Exit(1)
	}
	srv := &server.Server{Repo: repo, Log: logger}
	if *notifyURL != "" {
		srv.Notify = notify.NewHTTP(*notifyURL)
	}
	hs := &http.Server{
		Addr:              *listen,
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}()

	logger.Printf("serving repository %s, version %s, notifications %s",
		repo.Name(), server.Version, map[bool]string{true: "on", false: "off"}[srv.Notify != nil])
	if err := hs.ListenAndServe(); err != http.ErrServerClosed {
		logger.Print(pferr.Parsef("the listen address could not be used"))
		os.Exit(1)
	}
}

// discard drops the HTTP server's own log lines, which can name peers.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
