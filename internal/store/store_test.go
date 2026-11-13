package store

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestRefNameFixtures(t *testing.T) {
	var cases []struct{ Name, Error string }
	load(t, "refnames.json", &cases)
	for _, c := range cases {
		if got := errText(CheckRefName(c.Name)); got != c.Error {
			t.Errorf("%q: got %q, want %q", c.Name, got, c.Error)
		}
	}
}

func TestObjectFixtures(t *testing.T) {
	var cases []struct {
		Name, Kind, Content, OID, Error string
		Links                           []string
		Message                         *string
	}
	load(t, "objects.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			kind, ok := ParseType(c.Kind)
			if !ok {
				t.Fatalf("unknown kind %q", c.Kind)
			}
			links, err := Links(kind, []byte(c.Content))
			if errText(err) != c.Error {
				t.Fatalf("error %q, want %q", errText(err), c.Error)
			}
			if err != nil {
				return
			}
			got := []string{}
			for _, l := range links {
				got = append(got, l.Type.String()+" "+l.OID.String())
			}
			if !reflect.DeepEqual(got, c.Links) {
				t.Errorf("links %q, want %q", got, c.Links)
			}
			if oid := Hash(kind, []byte(c.Content)).String(); oid != c.OID {
				t.Errorf("object name %s, want %s", oid, c.OID)
			}
			if c.Message != nil {
				info, _ := ParseCommit([]byte(c.Content))
				if string(info.Message) != *c.Message {
					t.Errorf("message %q, want %q", info.Message, *c.Message)
				}
			}
		})
	}
}

func TestParseOID(t *testing.T) {
	for _, bad := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("A", 40), strings.Repeat("a", 41)} {
		if _, err := ParseOID(bad); errText(err) != "E_PARSE: an object name was not 40 lower-case hexadecimal digits" {
			t.Errorf("%q: got %v", bad, err)
		}
	}
	o, err := ParseOID(strings.Repeat("0", 40))
	if err != nil || !o.IsZero() {
		t.Fatalf("zero name: %v %v", o, err)
	}
}

func newRepo(t *testing.T) *Repo {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "example.git"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The exact files of a repository that Plainforge creates.
func TestOpenCreatesRepository(t *testing.T) {
	r := newRepo(t)
	if r.Name() != "example" {
		t.Fatalf("name %q", r.Name())
	}
	want := map[string]string{
		"HEAD":   "ref: refs/heads/main\n",
		"config": "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n",
	}
	for name, content := range want {
		b, err := os.ReadFile(filepath.Join(r.dir, name))
		if err != nil || string(b) != content {
			t.Errorf("%s: %q, %v", name, b, err)
		}
	}
	for _, d := range []string{"objects", "refs/heads", "refs/tags", "plainforge-tmp"} {
		if st, err := os.Stat(filepath.Join(r.dir, d)); err != nil || !st.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}
	if refs, err := r.Refs(); err != nil || len(refs) != 0 {
		t.Fatalf("refs %v, %v", refs, err)
	}
	// Opening it again changes nothing and works.
	if _, err := Open(r.dir); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup func(dir string)
		want  string
	}{
		{"packed objects", func(d string) {
			os.MkdirAll(filepath.Join(d, "objects/pack"), 0o755)
			os.WriteFile(filepath.Join(d, "objects/pack/pack-1.pack"), nil, 0o644)
		}, "E_NEEDS_SUBTOOL: the repository holds packed objects, which Plainforge cannot read yet"},
		{"packed refs", func(d string) { os.WriteFile(filepath.Join(d, "packed-refs"), nil, 0o644) },
			"E_NEEDS_SUBTOOL: the repository uses packed-refs, which Plainforge cannot read yet"},
		{"alternates", func(d string) {
			os.MkdirAll(filepath.Join(d, "objects/info"), 0o755)
			os.WriteFile(filepath.Join(d, "objects/info/alternates"), nil, 0o644)
		}, "E_NEEDS_SUBTOOL: the repository uses objects/info/alternates, which Plainforge cannot read yet"},
		{"sha256", func(d string) {
			os.WriteFile(filepath.Join(d, "config"), []byte("[core]\n\trepositoryformatversion = 1\n[extensions]\n\tobjectFormat = sha256\n"), 0o644)
		}, "E_NEEDS_SUBTOOL: the repository uses a format extension, which Plainforge cannot read yet"},
		{"detached HEAD", func(d string) {
			os.WriteFile(filepath.Join(d, "HEAD"), []byte(strings.Repeat("1", 40)+"\n"), 0o644)
		}, "E_NEEDS_SUBTOOL: HEAD does not name a branch"},
		{"no HEAD", func(d string) { os.Remove(filepath.Join(d, "HEAD")) }, "E_PARSE: the repository has no HEAD file"},
		{"no objects", func(d string) { os.RemoveAll(filepath.Join(d, "objects")) }, "E_PARSE: the repository has no objects directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRepo(t)
			c.setup(r.dir)
			if _, err := Open(r.dir); errText(err) != c.want {
				t.Fatalf("got %q, want %q", errText(err), c.want)
			}
		})
	}
	if _, err := Open(filepath.Join(t.TempDir(), "Bad Name.git")); errText(err) != "E_NEEDS_SUBTOOL: the repository directory name is outside the accepted character set" {
		t.Fatalf("bad name: %v", err)
	}
}

const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func commitOn(tree string, parents ...string) []byte {
	s := "tree " + tree + "\n"
	for _, p := range parents {
		s += "parent " + p + "\n"
	}
	return []byte(s + "author Example Author <author@example.invalid> 978307200 +0000\ncommitter Example Author <author@example.invalid> 978307200 +0000\n\nA made-up commit\n")
}

func TestQuarantineCommit(t *testing.T) {
	r := newRepo(t)
	q, err := r.NewQuarantine()
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := q.Write(Blob, []byte("Example widget notes\n"))
	tree, err := q.Write(Tree, append([]byte("100644 README.txt\x00"), blob[:]...))
	if err != nil {
		t.Fatal(err)
	}
	commit, err := q.Write(Commit, commitOn(tree.String()))
	if err != nil {
		t.Fatal(err)
	}
	if r.Has(commit) {
		t.Fatal("the object reached the repository before the quarantine was checked")
	}
	if typ, data, ok, err := q.Read(blob); !ok || err != nil || typ != Blob || string(data) != "Example widget notes\n" {
		t.Fatalf("read from quarantine: %v %q %v %v", typ, data, ok, err)
	}
	if err := q.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, o := range []OID{blob, tree, commit} {
		if !r.Has(o) {
			t.Fatalf("%s was not moved in", o)
		}
	}
	if typ, data, err := r.Read(blob); err != nil || typ != Blob || string(data) != "Example widget notes\n" {
		t.Fatalf("read back: %v %q %v", typ, data, err)
	}
	if typ, ok, err := r.TypeOf(tree); typ != Tree || !ok || err != nil {
		t.Fatalf("kind: %v %v %v", typ, ok, err)
	}
	if left, _ := os.ReadDir(filepath.Join(r.dir, workDir)); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}

	if err := r.SetRef("refs/heads/main", commit); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRef("refs/tags/v0.0.1", commit); err != nil {
		t.Fatal(err)
	}
	refs, err := r.Refs()
	want := []Ref{{"refs/heads/main", commit}, {"refs/tags/v0.0.1", commit}}
	if err != nil || !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs %v, %v", refs, err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.dir, "refs/heads/main")); string(b) != commit.String()+"\n" {
		t.Fatalf("ref file holds %q", b)
	}
	if err := r.SetRef("refs/heads/main/sub", commit); errText(err) != "E_NEEDS_SUBTOOL: the ref name collides with an existing ref" {
		t.Fatalf("colliding ref: %v", err)
	}

	// An ordinary Git accepts what was written.
	if git, err := exec.LookPath("git"); err == nil {
		cmd := exec.Command(git, "--git-dir", r.dir, "fsck", "--strict")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fsck: %v\n%s", err, out)
		}
	} else {
		t.Log("git is not installed; the git fsck check was not run")
	}
}

func TestQuarantineRefusals(t *testing.T) {
	missing := strings.Repeat("1", 40)
	cases := []struct {
		name  string
		write func(q *Quarantine) error
		want  string
	}{
		{"commit whose tree was not sent", func(q *Quarantine) error {
			_, err := q.Write(Commit, commitOn(missing))
			return err
		}, "E_PARSE: an object refers to an object that was not sent and is not stored"},
		{"commit whose parent was not sent", func(q *Quarantine) error {
			q.Write(Tree, nil)
			_, err := q.Write(Commit, commitOn(emptyTree, missing))
			return err
		}, "E_PARSE: an object refers to an object that was not sent and is not stored"},
		{"commit whose tree is a blob", func(q *Quarantine) error {
			blob, _ := q.Write(Blob, []byte("not a tree\n"))
			_, err := q.Write(Commit, commitOn(blob.String()))
			return err
		}, "E_PARSE: an object refers to an object of the wrong kind"},
		{"objects that disagree about a third", func(q *Quarantine) error {
			blob, _ := q.Write(Blob, []byte("file or directory?\n"))
			q.Write(Tree, append([]byte("100644 a\x00"), blob[:]...))
			_, err := q.Write(Tree, append([]byte("40000 a\x00"), blob[:]...))
			return err
		}, "E_PARSE: two objects disagree about the kind of a third"},
		{"malformed commit", func(q *Quarantine) error {
			_, err := q.Write(Commit, []byte("not a commit\n"))
			return err
		}, "E_PARSE: a commit did not start with its tree"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRepo(t)
			q, err := r.NewQuarantine()
			if err != nil {
				t.Fatal(err)
			}
			err = c.write(q)
			if err == nil {
				err = q.Commit()
			} else {
				q.Discard()
			}
			if errText(err) != c.want {
				t.Fatalf("got %q, want %q", errText(err), c.want)
			}
			if fans, _ := filepath.Glob(filepath.Join(r.dir, "objects", "??")); len(fans) != 0 {
				t.Fatalf("refused objects reached the repository: %v", fans)
			}
			if left, _ := os.ReadDir(filepath.Join(r.dir, workDir)); len(left) != 0 {
				t.Fatalf("temporary files left behind: %v", left)
			}
		})
	}
}

// A quarantine that was checked before a stop is finished on the next
// start; one that was not checked is thrown away.
func TestRecoveryAfterStop(t *testing.T) {
	r := newRepo(t)
	checked, _ := r.NewQuarantine()
	kept, _ := checked.Write(Blob, []byte("checked before the stop\n"))
	os.WriteFile(filepath.Join(checked.objs.dir, commitMarker), nil, 0o644)
	unchecked, _ := r.NewQuarantine()
	dropped, _ := unchecked.Write(Blob, []byte("never checked\n"))

	r2, err := Open(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Has(kept) {
		t.Error("the checked quarantine was not finished")
	}
	if r2.Has(dropped) {
		t.Error("the unchecked quarantine was accepted")
	}
	if left, _ := os.ReadDir(filepath.Join(r.dir, workDir)); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestDamagedObjectIsReported(t *testing.T) {
	r := newRepo(t)
	q, _ := r.NewQuarantine()
	a, _ := q.Write(Blob, []byte("first made-up file\n"))
	b, _ := q.Write(Blob, []byte("second made-up file\n"))
	if err := q.Commit(); err != nil {
		t.Fatal(err)
	}
	// Put the bytes of one object where the other should be.
	os.Chmod(r.objs.path(a), 0o644)
	data, _ := os.ReadFile(r.objs.path(b))
	os.WriteFile(r.objs.path(a), data, 0o644)
	if _, _, err := r.Read(a); errText(err) != "E_PARSE: a stored object is damaged" {
		t.Fatalf("swapped object: %v", err)
	}
	os.WriteFile(r.objs.path(b), []byte("not zlib"), 0o644)
	os.Chmod(r.objs.path(b), 0o644)
	os.WriteFile(r.objs.path(b), []byte("not zlib"), 0o644)
	if _, _, err := r.Read(b); errText(err) != "E_PARSE: a stored object is damaged" {
		t.Fatalf("garbage object: %v", err)
	}
	var none OID
	none[0] = 0xff
	if _, _, err := r.Read(none); errText(err) != "E_PARSE: the repository is missing an object it needs" {
		t.Fatalf("missing object: %v", err)
	}
}
