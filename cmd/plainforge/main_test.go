package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestParseFlagsAndBuildServer(t *testing.T) {
	none := func(string) string { return "" }

	if _, err := parseFlags(nil, none); err == nil {
		t.Fatal("expected error with neither -repo nor -repos")
	}
	if _, err := parseFlags([]string{"-repo", "/a", "-repos", "/b"}, none); err == nil {
		t.Fatal("expected error when both -repo and -repos are set")
	}
	if _, err := parseFlags([]string{"-repo", "/a", "-init-repos", "trawl"}, none); err == nil {
		t.Fatal("expected error when -init-repos is used without -repos")
	}

	root := filepath.Join(t.TempDir(), "repos")
	cfg, err := parseFlags([]string{
		"-repos", root,
		"-init-repos", "clearedstack.com,plainforge,trawl",
	}, func(k string) string {
		if k == "PLAINFORGE_MIRROR_URL" {
			return "https://example.invalid/ClearedStack"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mirrorURL != "https://example.invalid/ClearedStack" {
		t.Fatalf("mirrorURL = %q", cfg.mirrorURL)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logs bytes.Buffer
	srv, desc, err := buildServer(ctx, cfg, log.New(&logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if desc != "catalog" || srv.Catalog == nil || srv.Mirror == nil {
		t.Fatalf("unexpected server build: desc=%q cat=%v mirror=%v", desc, srv.Catalog, srv.Mirror)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz code = %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/clearedstack.com.git/info/refs?service=git-upload-pack", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("clearedstack.com info/refs code = %d", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/v1/repos/trawl/tasks?key=fp-1&open=true", nil))
	if rec3.Code != http.StatusOK || rec3.Body.String() != "{\"found\":false}\n" {
		t.Fatalf("/v1/repos/trawl/tasks = %d %q", rec3.Code, rec3.Body.String())
	}

	cfgTok, err := parseFlags([]string{"-repo", filepath.Join(t.TempDir(), "single.git")}, func(k string) string {
		if k == "PLAINFORGE_FORGE_TOKEN_FILE" {
			return "/var/run/secrets/plainforge/token"
		}
		return ""
	})
	if err != nil || cfgTok.forgeTokenFile != "/var/run/secrets/plainforge/token" {
		t.Fatalf("PLAINFORGE_FORGE_TOKEN_FILE parse: cfg=%+v err=%v", cfgTok, err)
	}
}
