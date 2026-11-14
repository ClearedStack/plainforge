package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ClearedStack/plainforge/internal/mirror"
	"github.com/ClearedStack/plainforge/internal/store"
)

func TestHealthz(t *testing.T) {
	_, hs, _, _ := newTestServer(t)

	resp, err := http.Get(hs.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "{\"status\":\"ok\"}\n" {
		t.Fatalf("GET /healthz: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	req, _ := http.NewRequest(http.MethodPost, hs.URL+"/healthz", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz status = %d, want 405", resp2.StatusCode)
	}
}

// TestMultiRepoAndGitHubMirror exercises hosting multiple repositories
// (including clearedstack.com), firing Keelworks notifications per repository,
// and push-mirroring source commits on main and tags outward while keeping
// internal commits working when the mirror is rate-limited.
func TestMultiRepoAndGitHubMirror(t *testing.T) {
	g := newGit(t)

	// Set up an upstream mirror server hosting bare mirror copies.
	mirrorCat, err := store.OpenCatalog(filepath.Join(t.TempDir(), "mirror-repos"), []string{"clearedstack.com", "trawl"})
	if err != nil {
		t.Fatal(err)
	}
	mirrorSrv := &Server{Catalog: mirrorCat}
	var rateLimited atomic.Bool
	mirrorHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rateLimited.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		mirrorSrv.ServeHTTP(w, r)
	}))
	defer mirrorHTTP.Close()

	// Set up primary Plainforge catalog and mirror queue.
	primaryCat, err := store.OpenCatalog(filepath.Join(t.TempDir(), "primary-repos"), []string{"clearedstack.com", "trawl"})
	if err != nil {
		t.Fatal(err)
	}
	pusher := mirror.NewHTTP(mirrorHTTP.URL)
	mq := mirror.NewQueue(pusher, nil)
	rec := &recorder{}
	primarySrv := &Server{Catalog: primaryCat, Notify: rec, Mirror: mq}
	primaryHTTP := httptest.NewServer(primarySrv)
	defer primaryHTTP.Close()

	// Push to clearedstack.com while the mirror is rate-limited (429).
	rateLimited.Store(true)
	g.ok("", "init", "-b", "main", "site")
	g.write("site/README.txt", "ClearedStack site notes\n")
	g.ok("site", "add", "-A")
	g.ok("site", "commit", "-m", "Initial site commit")
	siteFirst := g.ok("site", "rev-parse", "HEAD")
	g.ok("site", "branch", "draft-branch")
	g.ok("site", "tag", "-a", "v0.1.0", "-m", "First site tag")
	siteTag := g.ok("site", "rev-parse", "v0.1.0")
	g.ok("site", "push", primaryHTTP.URL+"/clearedstack.com.git", "main", "draft-branch", "v0.1.0")

	// Push to trawl as well.
	g.ok("", "init", "-b", "main", "trawl-work")
	g.write("trawl-work/main.go", "package main\n")
	g.ok("trawl-work", "add", "-A")
	g.ok("trawl-work", "commit", "-m", "Initial trawl commit")
	trawlFirst := g.ok("trawl-work", "rev-parse", "HEAD")
	g.ok("trawl-work", "push", primaryHTTP.URL+"/trawl.git", "main")

	// Internal pushes and Keelworks notifications succeeded despite the 429 mirror.
	wantNotes := []string{
		`{"repository":"clearedstack.com","ref":"refs/heads/main","before":"` + zeroName + `","after":"` + siteFirst + `"}` + "\n",
		`{"repository":"clearedstack.com","ref":"refs/tags/v0.1.0","before":"` + zeroName + `","after":"` + siteTag + `"}` + "\n",
		`{"repository":"trawl","ref":"refs/heads/main","before":"` + zeroName + `","after":"` + trawlFirst + `"}` + "\n",
	}
	if got := rec.take(); !reflect.DeepEqual(got, wantNotes) {
		t.Fatalf("notifications:\n%q\nwant\n%q", got, wantNotes)
	}
	if rem := mq.SyncOnce(context.Background()); rem != 2 {
		t.Fatalf("mirror pending while rate-limited = %d, want 2", rem)
	}

	// Lift the rate limit and flush the mirror queue.
	rateLimited.Store(false)
	if rem := mq.SyncOnce(context.Background()); rem != 0 {
		t.Fatalf("mirror pending after recovery = %d, want 0", rem)
	}

	// Verify the mirror has main and v0.1.0, and does NOT have draft-branch.
	gotSiteMirror := g.ok("", "ls-remote", mirrorHTTP.URL+"/clearedstack.com.git")
	wantSiteMirror := siteFirst + "\tHEAD\n" +
		siteFirst + "\trefs/heads/main\n" +
		siteTag + "\trefs/tags/v0.1.0\n" +
		siteFirst + "\trefs/tags/v0.1.0^{}"
	if gotSiteMirror != wantSiteMirror {
		t.Fatalf("mirror ls-remote:\n%s\nwant:\n%s", gotSiteMirror, wantSiteMirror)
	}

	// Incremental second commit with [skip-ci] mirrors to GitHub without notifying Keelworks.
	g.write("trawl-work/app.yaml", "name: trawl\ndigest: sha256:0000000000000000000000000000000000000000000000000000000000000000\n")
	g.ok("trawl-work", "add", "-A")
	g.ok("trawl-work", "commit", "-m", "Update trawl image digest [skip-ci]")
	trawlSecond := g.ok("trawl-work", "rev-parse", "HEAD")
	g.ok("trawl-work", "push", primaryHTTP.URL+"/trawl.git", "main")
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("[skip-ci] announced: %q", got)
	}
	if rem := mq.SyncOnce(context.Background()); rem != 0 {
		t.Fatalf("incremental mirror sync remaining = %d", rem)
	}
	if got := g.ok("", "ls-remote", mirrorHTTP.URL+"/trawl.git", "refs/heads/main"); got != trawlSecond+"\trefs/heads/main" {
		t.Fatalf("trawl mirror main = %q, want %s", got, trawlSecond)
	}

	for _, r := range mirrorCat.Repos() {
		g.ok("", "--git-dir", r.Dir(), "fsck", "--strict")
	}
}
