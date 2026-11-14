package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// gitRunner runs an ordinary Git client with a fixed, made-up identity and
// no access to the user's configuration or to any proxy.
type gitRunner struct {
	t   *testing.T
	bin string
	dir string
}

func newGit(t *testing.T) *gitRunner {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed; the tests that drive a real Git client were not run")
	}
	return &gitRunner{t: t, bin: bin, dir: t.TempDir()}
}

func (g *gitRunner) run(dir string, args ...string) (string, error) {
	cmd := exec.Command(g.bin, args...)
	cmd.Dir = filepath.Join(g.dir, dir)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + g.dir, "TZ=UTC", "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"NO_PROXY=*", "no_proxy=*",
		"GIT_AUTHOR_NAME=Example Author", "GIT_AUTHOR_EMAIL=author@example.invalid",
		"GIT_COMMITTER_NAME=Example Author", "GIT_COMMITTER_EMAIL=author@example.invalid",
		"GIT_AUTHOR_DATE=2001-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2001-01-01T00:00:00Z",
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (g *gitRunner) ok(dir string, args ...string) string {
	g.t.Helper()
	out, err := g.run(dir, args...)
	if err != nil {
		g.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (g *gitRunner) write(path, content string) {
	g.t.Helper()
	full := filepath.Join(g.dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func manyLines(n int, word string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("made-up line " + word + " " + strings.Repeat("x", i%40) + "\n")
	}
	return b.String()
}

// An ordinary "git push" and "git clone" work, what arrives is what was
// sent, later fetches bring only what is new, and each push that should be
// announced is announced.
func TestGitCloneAndPush(t *testing.T) {
	g := newGit(t)
	srv, hs, rec, _ := newTestServer(t)
	url := hs.URL + "/example.git"

	// Cloning the empty repository works.
	g.ok("", "clone", url, "empty")

	g.ok("", "init", "-b", "main", "work")
	g.write("work/README.txt", "Example widget notes\n")
	g.write("work/src/widget.txt", manyLines(400, "one"))
	g.write("work/src/deep/er/notes.txt", "nested\n")
	g.ok("work", "add", "-A")
	g.ok("work", "commit", "-m", "Add the example widget")
	first := g.ok("work", "rev-parse", "HEAD")
	g.ok("work", "push", url, "main")
	want := []string{`{"repository":"example","ref":"refs/heads/main","before":"` + zeroName + `","after":"` + first + `"}` + "\n"}
	if got := rec.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notifications after the first push: %q", got)
	}

	g.ok("", "clone", url, "copy")
	if got := g.ok("copy", "rev-parse", "HEAD"); got != first {
		t.Fatalf("clone is at %s, want %s", got, first)
	}
	if got := g.ok("copy", "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Fatalf("clone checked out %s", got)
	}
	g.ok("copy", "fsck", "--strict")
	b, err := os.ReadFile(filepath.Join(g.dir, "copy/src/widget.txt"))
	if err != nil || string(b) != manyLines(400, "one") {
		t.Fatalf("the cloned file differs from the pushed file: %v", err)
	}

	// A second push with a small change, a tag of each kind and a branch.
	g.write("work/src/widget.txt", manyLines(400, "one")+"one more line\n")
	g.ok("work", "commit", "-am", "Lengthen the example widget")
	second := g.ok("work", "rev-parse", "HEAD")
	g.ok("work", "tag", "-a", "v0.0.1", "-m", "First made-up release")
	g.ok("work", "tag", "light")
	g.ok("work", "branch", "side")
	tagName := g.ok("work", "rev-parse", "v0.0.1")
	g.ok("work", "push", url, "main", "v0.0.1", "light", "side")
	note := func(ref, before, after string) string {
		return `{"repository":"example","ref":"` + ref + `","before":"` + before + `","after":"` + after + `"}` + "\n"
	}
	want = []string{
		note("refs/heads/main", first, second),
		note("refs/tags/v0.0.1", zeroName, tagName),
		note("refs/tags/light", zeroName, second),
	}
	if got := rec.take(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notifications after the second push:\n%q\nwant\n%q", got, want)
	}

	// The existing clone fetches the difference.
	g.ok("copy", "pull", "--ff-only", "--tags")
	if got := g.ok("copy", "rev-parse", "HEAD"); got != second {
		t.Fatalf("after pull the clone is at %s, want %s", got, second)
	}
	if got := g.ok("copy", "rev-parse", "v0.0.1", "light", "origin/side"); got != tagName+"\n"+second+"\n"+second {
		t.Fatalf("tags and branch after pull: %q", got)
	}
	g.ok("copy", "fsck", "--strict")

	// A commit marked [skip-ci] lands and is not announced.
	g.write("work/app.yaml", "name: example\nimage: example@sha256:"+strings.Repeat("0", 64)+"\n")
	g.ok("work", "add", "-A")
	g.ok("work", "commit", "-m", "Update the example image digest [skip-ci]")
	third := g.ok("work", "rev-parse", "HEAD")
	g.ok("work", "push", url, "main")
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("a [skip-ci] push was announced: %q", got)
	}
	if got := g.ok("", "ls-remote", url, "refs/heads/main"); got != third+"\trefs/heads/main" {
		t.Fatalf("ls-remote: %q", got)
	}

	// The repository on disk passes Git's own consistency check.
	g.ok("", "--git-dir", srv.Repo.Dir(), "fsck", "--strict")
	if got := g.ok("", "--git-dir", srv.Repo.Dir(), "rev-list", "--count", "--all"); got != "3" {
		t.Fatalf("the repository holds %s commits, want 3", got)
	}
}

// What Plainforge has no rule for is refused, the reason reaches the Git
// client, and the repository is left as it was.
func TestGitRefusals(t *testing.T) {
	g := newGit(t)
	srv, hs, rec, _ := newTestServer(t)
	url := hs.URL + "/example.git"

	g.ok("", "init", "-b", "main", "work")
	g.write("work/a.txt", "first\n")
	g.ok("work", "add", "-A")
	g.ok("work", "commit", "-m", "First")
	g.write("work/a.txt", "second\n")
	g.ok("work", "commit", "-am", "Second")
	tip := g.ok("work", "rev-parse", "HEAD")
	g.ok("work", "tag", "v0.0.1")
	g.ok("work", "push", url, "main", "v0.0.1")
	rec.take()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"forced rewind of main", []string{"push", "--force", url, "HEAD~1:refs/heads/main"},
			"! [remote rejected] HEAD~1 -> main (E_NEEDS_SUBTOOL: the update is not a fast-forward)"},
		{"forced move of a tag", []string{"push", "--force", url, "HEAD~1:refs/tags/v0.0.1"},
			"! [remote rejected] HEAD~1 -> v0.0.1 (E_NEEDS_SUBTOOL: moving an existing tag is not supported)"},
		{"delete a branch", []string{"push", url, ":refs/heads/main"}, "! [remote rejected] main"},
		{"push outside branches and tags", []string{"push", url, "HEAD:refs/notes/commits"},
			"! [remote rejected] HEAD -> refs/notes/commits (E_NEEDS_SUBTOOL: only branches and tags are accepted)"},
		{"shallow clone", []string{"clone", "--depth", "1", url, "shallow"}, "Server does not support shallow clients"},
		{"another repository", []string{"ls-remote", hs.URL + "/other.git"}, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := g.run("work", c.args...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Fatalf("git %s:\nerror %v\n%s\nwant a failure that says %q", strings.Join(c.args, " "), err, out, c.want)
			}
		})
	}
	if got := g.ok("", "ls-remote", url); got != tip+"\tHEAD\n"+tip+"\trefs/heads/main\n"+tip+"\trefs/tags/v0.0.1" {
		t.Fatalf("the refusals changed the repository:\n%s", got)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("a refused update was announced: %q", got)
	}
	g.ok("", "--git-dir", srv.Repo.Dir(), "fsck", "--strict")
}

// A clone made by Plainforge from a history with merges and many commits
// matches the original, and a fetch across diverged history works.
func TestGitLargerHistory(t *testing.T) {
	g := newGit(t)
	_, hs, _, _ := newTestServer(t)
	url := hs.URL + "/example.git"

	g.ok("", "init", "-b", "main", "work")
	for i := 0; i < 40; i++ {
		g.write("work/file"+string(rune('a'+i%7))+".txt", manyLines(50+i, "round"))
		g.ok("work", "add", "-A")
		g.ok("work", "commit", "-m", "Round "+strings.Repeat("i", i+1))
	}
	g.ok("work", "checkout", "-b", "side", "HEAD~10")
	g.write("work/side.txt", "A side note\n")
	g.ok("work", "add", "-A")
	g.ok("work", "commit", "-m", "Add a side note")
	g.ok("work", "checkout", "main")
	g.ok("work", "merge", "--no-ff", "-m", "Join the side note", "side")
	g.ok("work", "push", url, "main")

	g.ok("", "clone", url, "copy")
	if a, b := g.ok("work", "rev-list", "--objects", "main"), g.ok("copy", "rev-list", "--objects", "main"); a != b {
		t.Fatal("the clone does not hold the same objects as the original")
	}

	// The copy commits on its own; the original moves on; the copy fetches.
	g.write("copy/local.txt", "only here\n")
	g.ok("copy", "add", "-A")
	g.ok("copy", "commit", "-m", "A commit only the copy has")
	for i := 0; i < 5; i++ {
		g.write("work/later.txt", manyLines(10+i, "later"))
		g.ok("work", "add", "-A")
		g.ok("work", "commit", "-m", "Later "+strings.Repeat("i", i+1))
	}
	g.ok("work", "push", url, "main")
	g.ok("copy", "fetch", "origin")
	if a, b := g.ok("work", "rev-parse", "main"), g.ok("copy", "rev-parse", "origin/main"); a != b {
		t.Fatalf("after fetch origin/main is %s, want %s", b, a)
	}
	g.ok("copy", "fsck", "--strict")

	// The copy cannot push over the newer history, but can after merging.
	if out, err := g.run("copy", "push", "origin", "main"); err == nil {
		t.Fatalf("a push that would lose commits was accepted:\n%s", out)
	}
	g.ok("copy", "merge", "-m", "Join the later work", "origin/main")
	g.ok("copy", "push", "origin", "main")
	if a, b := g.ok("copy", "rev-parse", "main"), g.ok("", "ls-remote", url, "refs/heads/main"); a+"\trefs/heads/main" != b {
		t.Fatalf("after the merge push the server has %q", b)
	}
}
