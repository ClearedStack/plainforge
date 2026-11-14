package forge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ClearedStack/plainforge/internal/forge"
	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/server"
	"github.com/ClearedStack/plainforge/internal/store"
)

type savedExchangeStep struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
	WantStatus  int    `json:"wantStatus"`
	WantBody    string `json:"wantBody"`
}

func TestReplayContractExamples(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contract", "v1", "examples", "forge_exchange.json"))
	if err != nil {
		t.Fatal(err)
	}
	var steps []savedExchangeStep
	if err := json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}

	cat, err := store.OpenCatalog(filepath.Join(t.TempDir(), "repos"), []string{"trawl", "clearedstack.com"})
	if err != nil {
		t.Fatal(err)
	}
	var mirrorTriggers atomic.Int32
	srv := &server.Server{
		Catalog: cat,
		Mirror: mirrorFunc(func(*store.Repo) {
			mirrorTriggers.Add(1)
		}),
	}
	hs := httptest.NewServer(srv)
	defer hs.Close()

	for _, step := range steps {
		t.Run(step.Name, func(t *testing.T) {
			var body io.Reader
			if step.Body != "" {
				body = strings.NewReader(step.Body)
			}
			req, err := http.NewRequest(step.Method, hs.URL+step.Path, body)
			if err != nil {
				t.Fatal(err)
			}
			if step.ContentType != "" {
				req.Header.Set("Content-Type", step.ContentType)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			gotBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != step.WantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, step.WantStatus, gotBody)
			}
			if string(gotBody) != step.WantBody {
				t.Fatalf("body:\n%s\nwant:\n%s", gotBody, step.WantBody)
			}
		})
	}
	if got := mirrorTriggers.Load(); got != 1 {
		t.Fatalf("mirror triggers = %d, want 1", got)
	}
}

type mirrorFunc func(*store.Repo)

func (f mirrorFunc) Enqueue(r *store.Repo) { f(r) }

func TestClientRecoveryAuthAndGitFsck(t *testing.T) {
	reposRoot := filepath.Join(t.TempDir(), "repos")
	cat, err := store.OpenCatalog(reposRoot, []string{"clearedstack.com"})
	if err != nil {
		t.Fatal(err)
	}
	tokFile := filepath.Join(t.TempDir(), "forge-token")
	if err := os.WriteFile(tokFile, []byte("token-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &server.Server{Catalog: cat, ForgeTokenFile: tokFile}
	hs := httptest.NewServer(srv)
	defer hs.Close()

	// Unauthenticated call is rejected 401 E_PARSE when ForgeTokenFile is set.
	unauth := forge.NewClient(hs.URL)
	if _, _, err := unauth.FindOpenTask(context.Background(), "clearedstack.com", "k1"); err == nil {
		t.Fatal("expected unauthenticated FindOpenTask to fail")
	} else {
		var pe *pferr.Error
		if !errors.As(err, &pe) || pe.Code != pferr.Parse {
			t.Fatalf("expected E_PARSE, got %v", err)
		}
	}

	// Authenticated client succeeds and survives token rotation.
	cli := forge.NewClient(hs.URL)
	cli.BearerToken = "token-v1"
	task, err := cli.OpenTask(context.Background(), "clearedstack.com", forge.TaskDraft{
		Key:   "fp-site",
		Title: "chartroom: E_NEEDS_SUBTOOL [fp-site]",
		Body:  "First seen on item: PA-0001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "clearedstack.com#1" || task.Occurrences != 1 {
		t.Fatalf("unexpected task: %+v", task)
	}

	if err := os.WriteFile(tokFile, []byte("token-v2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli.BearerToken = "token-v2"
	task, err = cli.AddOccurrence(context.Background(), "clearedstack.com", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2", task.Occurrences)
	}

	// Seed refs/heads/main via git push when git is installed, then propose
	// changes exercising tricky Git tree sort order (foo-bar < foo.txt < foo/ < foo0).
	gitBin, gitErr := exec.LookPath("git")
	if gitErr == nil {
		work := t.TempDir()
		runGit(t, gitBin, work, "init", "-b", "main", "seed")
		seedDir := filepath.Join(work, "seed")
		writeTestFile(t, filepath.Join(seedDir, "foo-bar"), "dash\n")
		writeTestFile(t, filepath.Join(seedDir, "foo.txt"), "dot\n")
		writeTestFile(t, filepath.Join(seedDir, "foo0"), "zero\n")
		writeTestFile(t, filepath.Join(seedDir, "src", "page.mdx"), "old content\n")
		runGit(t, gitBin, seedDir, "add", "-A")
		runGit(t, gitBin, seedDir, "-c", "user.name=Test", "-c", "user.email=test@clearedstack.internal", "commit", "-m", "Seed site")
		runGit(t, gitBin, seedDir, "push", hs.URL+"/clearedstack.com.git", "main")
	}

	// Propose change 1: creates directory "foo/nested.json" alongside foo-bar, foo.txt, foo0.
	created, err := cli.ProposeChange(context.Background(), forge.Change{
		Repo:    "clearedstack.com",
		Branch:  "edit/EP-0001",
		Path:    "foo/nested.json",
		Content: "{\"ok\":true}\n",
		Title:   "Portal proposal EP-0001: foo",
		Message: "Portal proposal EP-0001: foo\n\nClass: DATA\nTarget: foo",
	})
	if err != nil || !created {
		t.Fatalf("ProposeChange 1: created=%v err=%v", created, err)
	}

	// Propose change 2: replaces existing src/page.mdx on its own branch.
	created2, err := cli.ProposeChange(context.Background(), forge.Change{
		Repo:    "clearedstack.com",
		Branch:  "edit/EP-0002",
		Path:    "src/page.mdx",
		Content: "updated content\n",
		Title:   "Portal proposal EP-0002: page",
		Message: "Portal proposal EP-0002: page\n\nClass: DATA\nTarget: page",
	})
	if err != nil || !created2 {
		t.Fatalf("ProposeChange 2: created=%v err=%v", created2, err)
	}

	// Simulate pod restart: open a fresh Catalog and Server over the same directory.
	cat2, err := store.OpenCatalog(reposRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	hs2 := httptest.NewServer(&server.Server{Catalog: cat2})
	defer hs2.Close()

	cli2 := forge.NewClient(hs2.URL)
	foundTask, ok, err := cli2.FindOpenTask(context.Background(), "clearedstack.com", "fp-site")
	if err != nil || !ok || foundTask.Occurrences != 2 {
		t.Fatalf("after restart FindOpenTask: ok=%v task=%+v err=%v", ok, foundTask, err)
	}
	again, err := cli2.ProposeChange(context.Background(), forge.Change{
		Repo:    "clearedstack.com",
		Branch:  "edit/EP-0002",
		Path:    "src/page.mdx",
		Content: "updated content\n",
		Title:   "Portal proposal EP-0002: page",
		Message: "Portal proposal EP-0002: page\n\nClass: DATA\nTarget: page",
	})
	if err != nil || again {
		t.Fatalf("after restart idempotent ProposeChange: created=%v err=%v", again, err)
	}

	if gitErr == nil {
		siteRepo, _ := cat2.Get("clearedstack.com")
		runGit(t, gitBin, "", "--git-dir", siteRepo.Dir(), "fsck", "--strict")

		cloneParent := t.TempDir()
		runGit(t, gitBin, cloneParent, "clone", "-b", "edit/EP-0002", hs2.URL+"/clearedstack.com.git", "check")
		got, err := os.ReadFile(filepath.Join(cloneParent, "check", "src", "page.mdx"))
		if err != nil || string(got) != "updated content\n" {
			t.Fatalf("cloned edit/EP-0002 src/page.mdx = %q, err = %v", got, err)
		}
		// Ensure foo-bar from main is preserved intact on edit/EP-0002!
		gotDash, err := os.ReadFile(filepath.Join(cloneParent, "check", "foo-bar"))
		if err != nil || string(gotDash) != "dash\n" {
			t.Fatalf("cloned edit/EP-0002 foo-bar = %q, err = %v", gotDash, err)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, gitBin, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(gitBin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = []string{
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"PATH=" + os.Getenv("PATH"),
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
