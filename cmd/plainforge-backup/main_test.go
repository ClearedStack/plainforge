package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/plainforge/internal/age"
	"github.com/ClearedStack/plainforge/internal/store"
)

func TestCronJobArgsEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	reposDir := filepath.Join(tmp, "repos")
	scratchDir := filepath.Join(tmp, "scratch")
	if _, err := store.OpenCatalog(reposDir, []string{"clearedstack.com", "plainforge"}); err != nil {
		t.Fatal(err)
	}

	// 1. Generate an age keypair using the keygen subcommand.
	idFile := filepath.Join(tmp, "identity.txt")
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	if err := run(context.Background(), []string{"keygen", "-output", idFile}, func(string) string { return "" }, &bytes.Buffer{}, logger); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	idBytes, err := os.ReadFile(idFile)
	if err != nil {
		t.Fatal(err)
	}
	id, err := age.ParseIdentityFile(idBytes)
	if err != nil {
		t.Fatalf("ParseIdentityFile: %v", err)
	}
	recFile := filepath.Join(tmp, "recipient")
	if err := os.WriteFile(recFile, []byte(id.Recipient().String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 2. Exercise exact flag shape from deploy/backup-cronjob.yaml (backup CronJob)
	// against a loopback destination URL from PLAINFORGE_BACKUP_URL.
	var received int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	outArchive := filepath.Join(tmp, "saved.tar.gz.age")
	err = run(context.Background(), []string{
		"backup",
		"-repos=" + reposDir,
		"-scratch=" + scratchDir,
		"-recipient-file=" + recFile,
		"-dest-token-file=" + filepath.Join(tmp, "missing-optional-token"),
		"-output=" + outArchive,
	}, func(k string) string {
		if k == "PLAINFORGE_BACKUP_URL" {
			return srv.URL + "/nightly.age"
		}
		return ""
	}, &bytes.Buffer{}, logger)
	if err != nil {
		t.Fatalf("backup cronjob args: %v", err)
	}
	if received != 1 {
		t.Fatalf("received = %d, want 1", received)
	}

	// 3. Exercise exact flag shape from deploy/backup-cronjob.yaml (restore-drill CronJob).
	err = run(context.Background(), []string{
		"restore-drill",
		"-repos=" + reposDir,
		"-scratch=" + scratchDir,
		"-recipient-file=" + recFile,
	}, func(string) string { return "" }, &bytes.Buffer{}, logger)
	if err != nil {
		t.Fatalf("restore-drill cronjob args: %v", err)
	}

	// 4. Also exercise external-archive restore-drill mode.
	err = run(context.Background(), []string{
		"restore-drill",
		"-scratch=" + scratchDir,
		"-archive=" + outArchive,
		"-identity-file=" + idFile,
	}, func(string) string { return "" }, &bytes.Buffer{}, logger)
	if err != nil {
		t.Fatalf("restore-drill external archive: %v", err)
	}

	// Ensure log lines contain no file paths or URLs.
	logText := logs.String()
	if strings.Contains(logText, tmp) || strings.Contains(logText, srv.URL) {
		t.Fatalf("logs leaked path or URL: %q", logText)
	}
}
