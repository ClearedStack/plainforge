package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ClearedStack/plainforge/internal/forge"
	"github.com/ClearedStack/plainforge/internal/mirror"
	"github.com/ClearedStack/plainforge/internal/notify"
	"github.com/ClearedStack/plainforge/internal/store"
)

// TestClusterPeersWireSuite replays the end-to-end cluster wire flows of
// Keelworks, Bellbuoy, and Shipkeeper against a multi-repository Plainforge
// server whose seed repository starts packed via git gc.
func TestClusterPeersWireSuite(t *testing.T) {
	g := newGit(t)
	tmp := t.TempDir()

	// 1. Seed plainforge.git via git clone --bare + git gc --aggressive + pack-refs
	// so the repository starts with PACKv2 deltas and packed-refs on disk.
	g.ok("", "init", "-b", "main", "seed-src")
	g.write("seed-src/app.yaml", "name: plainforge\nnamespace: clearedstack\nimage: plainforge\ndigest: sha256:"+strings.Repeat("0", 64)+"\n")
	g.write("seed-src/data/rates.json", "{\"tier\":\"baseline\",\"rate\":100}\n")
	g.write("seed-src/README.txt", manyLines(120, "seed"))
	g.ok("seed-src", "add", "-A")
	g.ok("seed-src", "commit", "-m", "Seed plainforge repository")
	seedTip := g.ok("seed-src", "rev-parse", "HEAD")

	reposRoot := filepath.Join(tmp, "primary-repos")
	if err := os.MkdirAll(reposRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	seededBare := filepath.Join(reposRoot, "plainforge.git")
	g.ok("", "clone", "--bare", filepath.Join(g.dir, "seed-src"), seededBare)
	g.ok("", "--git-dir", seededBare, "gc", "--aggressive", "--prune=now")
	g.ok("", "--git-dir", seededBare, "pack-refs", "--all")

	// Set up mock Keelworks /webhook receiver verifying projected SA bearer auth.
	const wantSAToken = "projected-keelworks-sa-jwt"
	saTokenFile := filepath.Join(tmp, "keelworks-token")
	if err := os.WriteFile(saTokenFile, []byte(wantSAToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var kwMu sync.Mutex
	var kwEvents []string
	keelworksHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/webhook" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+wantSAToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		kwMu.Lock()
		kwEvents = append(kwEvents, string(b))
		kwMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer keelworksHTTP.Close()

	takeWebhooks := func() []string {
		kwMu.Lock()
		defer kwMu.Unlock()
		out := append([]string(nil), kwEvents...)
		kwEvents = nil
		return out
	}

	// Set up outbound GitHub mirror server.
	mirrorCat, err := store.OpenCatalog(filepath.Join(tmp, "mirror-repos"), []string{"plainforge", "clearedstack.com"})
	if err != nil {
		t.Fatal(err)
	}
	mirrorHTTP := httptest.NewServer(&Server{Catalog: mirrorCat})
	defer mirrorHTTP.Close()

	// Open Primary Plainforge catalog (seeded packed plainforge.git + empty trawl.git).
	primaryCat, err := store.OpenCatalog(reposRoot, []string{"plainforge", "clearedstack.com", "trawl"})
	if err != nil {
		t.Fatalf("OpenCatalog with packed seed repo: %v", err)
	}
	notifySender := notify.NewHTTP(keelworksHTTP.URL + "/webhook")
	notifySender.TokenFile = saTokenFile
	notifySpool, err := notify.OpenSpool(filepath.Join(primaryCat.SpoolDir(), "notify"), notifySender, nil)
	if err != nil {
		t.Fatal(err)
	}
	mq := mirror.NewQueue(mirror.NewHTTP(mirrorHTTP.URL), nil)

	const wantForgeToken = "bellbuoy-forge-bearer-token"
	forgeTokenFile := filepath.Join(tmp, "forge-token")
	if err := os.WriteFile(forgeTokenFile, []byte(wantForgeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	primarySrv := &Server{
		Catalog:        primaryCat,
		Notify:         notifySpool,
		Mirror:         mq,
		ForgeTokenFile: forgeTokenFile,
	}
	primaryHTTP := httptest.NewServer(primarySrv)
	defer primaryHTTP.Close()

	// 2. Keelworks loop:
	//    (a) Developer clones packed plainforge.git over smart HTTP and pushes a source commit.
	g.ok("", "clone", primaryHTTP.URL+"/plainforge.git", "dev-work")
	g.write("dev-work/README.txt", manyLines(120, "seed")+"updated line\n")
	g.ok("dev-work", "add", "-A")
	g.ok("dev-work", "commit", "-m", "Improve README documentation")
	devCommit := g.ok("dev-work", "rev-parse", "HEAD")
	g.ok("dev-work", "push", "origin", "main")

	notifySpool.Flush(context.Background())
	wantFirstHook := []string{
		`{"repository":"plainforge","ref":"refs/heads/main","before":"` + seedTip + `","after":"` + devCommit + `"}` + "\n",
	}
	if got := takeWebhooks(); !reflect.DeepEqual(got, wantFirstHook) {
		t.Fatalf("Keelworks webhooks = %q, want %q", got, wantFirstHook)
	}

	//    (b) Keelworks checks out devCommit, pins digest in app.yaml with [skip-ci], and pushes.
	pinnedDigest := "sha256:" + strings.Repeat("1", 64)
	g.ok("", "clone", primaryHTTP.URL+"/plainforge.git", "keelworks-pin")
	g.ok("keelworks-pin", "checkout", devCommit)
	g.write("keelworks-pin/app.yaml", "name: plainforge\nnamespace: clearedstack\nimage: plainforge\ndigest: "+pinnedDigest+"\n")
	g.ok("keelworks-pin", "add", "app.yaml")
	g.ok("keelworks-pin", "commit", "-m", "keelworks: pin plainforge image digest [skip-ci]")
	pinnedCommit := g.ok("keelworks-pin", "rev-parse", "HEAD")
	g.ok("keelworks-pin", "push", "origin", "HEAD:refs/heads/main")

	notifySpool.Flush(context.Background())
	if got := takeWebhooks(); len(got) != 0 {
		t.Fatalf("[skip-ci] pin unexpectedly notified Keelworks: %q", got)
	}

	// 3. Bellbuoy loop:
	//    (a) Smart HTTP orphan refs/heads/bellbuoy/tasks creation + incremental fetch + update.
	g.ok("", "init", "bb-tasks")
	g.ok("bb-tasks", "checkout", "--orphan", "bellbuoy/tasks")
	g.write("bb-tasks/tasks.json", "{\"tasks\":[{\"id\":\"T-1\",\"key\":\"fp-1\",\"occurrences\":1,\"open\":true}]}\n")
	g.ok("bb-tasks", "add", "tasks.json")
	g.ok("bb-tasks", "commit", "-m", "bellbuoy: open T-1")
	g.ok("bb-tasks", "push", primaryHTTP.URL+"/plainforge.git", "HEAD:refs/heads/bellbuoy/tasks")

	g.ok("", "init", "bb-tasks-next")
	g.ok("bb-tasks-next", "fetch", primaryHTTP.URL+"/plainforge.git", "refs/heads/bellbuoy/tasks:refs/heads/bellbuoy/tasks")
	g.ok("bb-tasks-next", "checkout", "bellbuoy/tasks")
	g.write("bb-tasks-next/tasks.json", "{\"tasks\":[{\"id\":\"T-1\",\"key\":\"fp-1\",\"occurrences\":2,\"open\":true}]}\n")
	g.ok("bb-tasks-next", "add", "tasks.json")
	g.ok("bb-tasks-next", "commit", "-m", "bellbuoy: occurrence 2 for T-1")
	g.ok("bb-tasks-next", "push", primaryHTTP.URL+"/plainforge.git", "HEAD:refs/heads/bellbuoy/tasks")

	//    (b) Smart HTTP edit branch proposal refs/heads/edit/EP-0001.
	g.ok("", "clone", "--branch", "main", primaryHTTP.URL+"/plainforge.git", "bb-edit")
	g.ok("bb-edit", "checkout", "-b", "edit/EP-0001")
	g.write("bb-edit/data/rates.json", "{\"tier\":\"baseline\",\"rate\":105}\n")
	g.ok("bb-edit", "add", "data/rates.json")
	g.ok("bb-edit", "commit", "-m", "bellbuoy: propose EP-0001 rate update")
	g.ok("bb-edit", "push", "origin", "refs/heads/edit/EP-0001")

	//    (c) Forge v1 HTTP API (/v1/repos/plainforge/...) ProposeChange + SyncMirror.
	fc := &forge.Client{
		BaseURL:     primaryHTTP.URL,
		BearerToken: wantForgeToken,
		HTTPClient:  primaryHTTP.Client(),
	}
	created, err := fc.ProposeChange(context.Background(), forge.Change{
		Repo:    "plainforge",
		Branch:  "edit/EP-0002",
		Path:    "data/rates.json",
		Content: "{\"tier\":\"baseline\",\"rate\":110}\n",
		Title:   "Update rate table EP-0002",
		Message: "bellbuoy: propose EP-0002 via Forge v1\n",
	})
	if err != nil || !created {
		t.Fatalf("Forge v1 ProposeChange on packed+loose repo: created=%v err=%v", created, err)
	}
	if err := fc.SyncMirror(context.Background(), "plainforge", []string{forge.ContentSource, forge.ContentTask}); err != nil {
		t.Fatalf("Forge v1 SyncMirror: %v", err)
	}

	// Confirm Bellbuoy branches neither notified Keelworks nor leaked to the GitHub mirror.
	notifySpool.Flush(context.Background())
	if got := takeWebhooks(); len(got) != 0 {
		t.Fatalf("Bellbuoy branches unexpectedly notified Keelworks: %q", got)
	}
	if rem := mq.SyncOnce(context.Background()); rem != 0 {
		t.Fatalf("mirror SyncOnce remaining = %d, want 0", rem)
	}
	mirrorRefs := g.ok("", "ls-remote", mirrorHTTP.URL+"/plainforge.git")
	wantMirrorRefs := pinnedCommit + "\tHEAD\n" + pinnedCommit + "\trefs/heads/main"
	if mirrorRefs != wantMirrorRefs {
		t.Fatalf("outbound mirror refs:\n%s\nwant:\n%s", mirrorRefs, wantMirrorRefs)
	}

	// 4. Shipkeeper reconciliation poll:
	//    (a) Empty repository trawl.git returns 200 and zero refs cleanly.
	emptyRefs := g.ok("", "ls-remote", primaryHTTP.URL+"/trawl.git")
	if emptyRefs != "" {
		t.Fatalf("expected empty trawl.git ls-remote, got %q", emptyRefs)
	}
	//    (b) Populated repository plainforge.git serves the pinned app.yaml on main.
	g.ok("", "clone", "--branch", "main", primaryHTTP.URL+"/plainforge.git", "shipkeeper-read")
	appBytes, err := os.ReadFile(filepath.Join(g.dir, "shipkeeper-read", "app.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appBytes), "digest: "+pinnedDigest) {
		t.Fatalf("Shipkeeper read unexpected app.yaml:\n%s", appBytes)
	}

	// 5. Every repository in both primary and mirror catalogs passes both pure-Go
	// Repo.Fsck() and git fsck --strict.
	for _, cat := range []*store.Catalog{primaryCat, mirrorCat} {
		for _, r := range cat.Repos() {
			if _, err := r.Fsck(); err != nil {
				t.Fatalf("Repo.Fsck(%s): %v", r.Name(), err)
			}
			g.ok("", "--git-dir", r.Dir(), "fsck", "--strict")
		}
	}
}
