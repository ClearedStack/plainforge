package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/clearedstack/plainforge/internal/pferr"
)

// DefaultBranch is the branch HEAD names in a repository Plainforge creates.
const DefaultBranch = "refs/heads/main"

// workDir is the one directory inside the repository that Plainforge uses
// for its own temporary files. Git tools ignore it.
const workDir = "plainforge-tmp"

// commitMarker is the file that says a quarantine was fully checked and was
// being moved into the repository.
const commitMarker = "checked"

var (
	repoNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	refPartRE  = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*$`)
)

// Repo is one bare repository on disk.
type Repo struct {
	dir  string
	name string
	objs objdir
}

// Ref is a ref and the object it names.
type Ref struct {
	Name string
	OID  OID
}

// Open opens the bare repository in dir. A missing or empty directory is
// set up as a new, empty repository. A repository that uses a feature
// Plainforge has no rule for is refused.
func Open(dir string) (*Repo, error) {
	name := strings.TrimSuffix(filepath.Base(filepath.Clean(dir)), ".git")
	if !repoNameRE.MatchString(name) {
		return nil, pferr.Needsf("the repository directory name is outside the accepted character set")
	}
	r := &Repo{dir: dir, name: name, objs: objdir{filepath.Join(dir, "objects")}}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, storageErr("opening the repository", err)
	}
	if len(entries) == 0 {
		if err := r.create(); err != nil {
			return nil, err
		}
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	if err := r.recover(); err != nil {
		return nil, err
	}
	return r, nil
}

// Name is the repository name, taken from its directory.
func (r *Repo) Name() string { return r.name }

func (r *Repo) create() error {
	for _, d := range []string{"objects", "refs/heads", "refs/tags"} {
		if err := os.MkdirAll(filepath.Join(r.dir, d), 0o755); err != nil {
			return storageErr("creating the repository", err)
		}
	}
	files := map[string]string{
		"HEAD":   "ref: " + DefaultBranch + "\n",
		"config": "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n",
	}
	for _, name := range []string{"config", "HEAD"} {
		if err := os.WriteFile(filepath.Join(r.dir, name), []byte(files[name]), 0o644); err != nil {
			return storageErr("creating the repository", err)
		}
	}
	return nil
}

// check refuses repositories that Plainforge cannot serve correctly yet.
func (r *Repo) check() error {
	if _, err := r.HeadTarget(); err != nil {
		return err
	}
	if st, err := os.Stat(r.objs.dir); err != nil || !st.IsDir() {
		return pferr.Parsef("the repository has no objects directory")
	}
	if packs, _ := filepath.Glob(filepath.Join(r.objs.dir, "pack", "*.pack")); len(packs) > 0 {
		return pferr.Needsf("the repository holds packed objects, which Plainforge cannot read yet")
	}
	for _, f := range []string{"packed-refs", "shallow", "objects/info/alternates"} {
		if _, err := os.Stat(filepath.Join(r.dir, f)); err == nil {
			return pferr.Needsf("the repository uses %s, which Plainforge cannot read yet", f)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(r.dir, "config"))
	if err != nil {
		return pferr.Parsef("the repository has no config file")
	}
	lower := strings.ToLower(string(cfg))
	if strings.Contains(lower, "objectformat") || strings.Contains(lower, "[extensions") {
		return pferr.Needsf("the repository uses a format extension, which Plainforge cannot read yet")
	}
	return nil
}

// recover clears the temporary directory. A quarantine that had been fully
// checked before the process stopped is moved into the repository; any
// other leftover is thrown away.
func (r *Repo) recover() error {
	work := filepath.Join(r.dir, workDir)
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		p := filepath.Join(work, e.Name())
		if _, err := os.Stat(filepath.Join(p, commitMarker)); err == nil && e.IsDir() {
			if err := r.moveIn(objdir{p}); err != nil {
				return err
			}
		}
		if err := os.RemoveAll(p); err != nil {
			return storageErr("clearing temporary files", err)
		}
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return storageErr("creating the temporary directory", err)
	}
	return nil
}

// HeadTarget returns the branch that HEAD names.
func (r *Repo) HeadTarget() (string, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, "HEAD"))
	if err != nil {
		return "", pferr.Parsef("the repository has no HEAD file")
	}
	target, ok := strings.CutPrefix(strings.TrimSuffix(string(b), "\n"), "ref: ")
	if !ok {
		return "", pferr.Needsf("HEAD does not name a branch")
	}
	if err := CheckRefName(target); err != nil || !strings.HasPrefix(target, "refs/heads/") {
		return "", pferr.Needsf("HEAD does not name a branch")
	}
	return target, nil
}

// CheckRefName accepts a branch or tag name made of plain characters and
// refuses everything else.
func CheckRefName(name string) error {
	rest, ok := strings.CutPrefix(name, "refs/heads/")
	if !ok {
		rest, ok = strings.CutPrefix(name, "refs/tags/")
	}
	if !ok {
		return pferr.Needsf("only branches and tags are accepted")
	}
	if len(name) > 200 {
		return pferr.Needsf("the ref name is longer than 200 characters")
	}
	for _, part := range strings.Split(rest, "/") {
		if !refPartRE.MatchString(part) || strings.HasSuffix(part, ".") ||
			strings.HasSuffix(part, ".lock") || strings.Contains(part, "..") {
			return pferr.Needsf("the ref name is outside the accepted character set")
		}
	}
	return nil
}

// Dir is the directory the repository lives in.
func (r *Repo) Dir() string { return r.dir }

// Has reports whether an object is stored.
func (r *Repo) Has(o OID) bool { return r.objs.has(o) }

// Read returns a stored object.
func (r *Repo) Read(o OID) (Type, []byte, error) {
	t, data, err := r.objs.read(o)
	if errors.Is(err, errNotFound) {
		return 0, nil, pferr.Parsef("the repository is missing an object it needs")
	}
	return t, data, err
}

// TypeOf returns the kind of a stored object. ok is false when the object
// is not stored.
func (r *Repo) TypeOf(o OID) (t Type, ok bool, err error) {
	t, err = r.objs.typeOf(o)
	if errors.Is(err, errNotFound) {
		return 0, false, nil
	}
	return t, err == nil, err
}

// Refs returns every branch and tag, sorted by name.
func (r *Repo) Refs() ([]Ref, error) {
	var refs []Ref
	root := filepath.Join(r.dir, "refs")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return storageErr("listing refs", err)
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(r.dir, p)
		name := filepath.ToSlash(rel)
		if err := CheckRefName(name); err != nil {
			return pferr.Needsf("the repository holds a ref Plainforge has no rule for")
		}
		oid, _, err := r.Ref(name)
		if err != nil {
			return err
		}
		refs = append(refs, Ref{Name: name, OID: oid})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs, nil
}

// Ref returns the object one ref names. ok is false when there is no such
// ref.
func (r *Repo) Ref(name string) (oid OID, ok bool, err error) {
	if err := CheckRefName(name); err != nil {
		return oid, false, err
	}
	b, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(name)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return oid, false, nil
		}
		// A directory here means other refs live below this name.
		if st, serr := os.Stat(filepath.Join(r.dir, filepath.FromSlash(name))); serr == nil && st.IsDir() {
			return oid, false, nil
		}
		return oid, false, storageErr("reading a ref", err)
	}
	oid, err = ParseOID(strings.TrimSuffix(string(b), "\n"))
	if err != nil {
		return oid, false, pferr.Parsef("a stored ref is damaged")
	}
	return oid, true, nil
}

// SetRef points a ref at an object. The new value is written to a temporary
// file, flushed to disk and moved into place.
func (r *Repo) SetRef(name string, oid OID) error {
	if err := CheckRefName(name); err != nil {
		return err
	}
	final := filepath.Join(r.dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return pferr.Needsf("the ref name collides with an existing ref")
	}
	if st, err := os.Stat(final); err == nil && st.IsDir() {
		return pferr.Needsf("the ref name collides with an existing ref")
	}
	tmp, err := os.CreateTemp(filepath.Join(r.dir, workDir), "ref-*")
	if err != nil {
		return storageErr("writing a ref", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(oid.String() + "\n")
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), final)
	}
	if err == nil {
		err = syncFile(filepath.Dir(final))
	}
	if err != nil {
		return storageErr("writing a ref", err)
	}
	return nil
}

// moveIn moves every object of a checked quarantine into the repository.
func (r *Repo) moveIn(q objdir) error {
	fans, err := os.ReadDir(q.dir)
	if err != nil {
		return storageErr("accepting objects", err)
	}
	for _, fan := range fans {
		if !fan.IsDir() || len(fan.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(q.dir, fan.Name()))
		if err != nil {
			return storageErr("accepting objects", err)
		}
		if err := os.MkdirAll(filepath.Join(r.objs.dir, fan.Name()), 0o755); err != nil {
			return storageErr("accepting objects", err)
		}
		for _, f := range files {
			from := filepath.Join(q.dir, fan.Name(), f.Name())
			if err := os.Rename(from, filepath.Join(r.objs.dir, fan.Name(), f.Name())); err != nil {
				return storageErr("accepting objects", err)
			}
		}
		if err := syncFile(filepath.Join(r.objs.dir, fan.Name())); err != nil {
			return storageErr("accepting objects", err)
		}
	}
	return nil
}

// Quarantine holds the objects of one push apart from the repository until
// all of them have been checked.
type Quarantine struct {
	repo  *Repo
	objs  objdir
	types map[OID]Type // every object the push sent
	needs map[OID]Type // every object those objects refer to
}

// NewQuarantine starts an empty quarantine.
func (r *Repo) NewQuarantine() (*Quarantine, error) {
	dir, err := os.MkdirTemp(filepath.Join(r.dir, workDir), "push-*")
	if err != nil {
		return nil, storageErr("preparing to receive objects", err)
	}
	return &Quarantine{repo: r, objs: objdir{dir}, types: map[OID]Type{}, needs: map[OID]Type{}}, nil
}

// Write adds an object to the quarantine. Its content is parsed, so a
// malformed commit, tree or tag is refused here.
func (q *Quarantine) Write(t Type, data []byte) (OID, error) {
	links, err := Links(t, data)
	if err != nil {
		return OID{}, err
	}
	oid := Hash(t, data)
	if _, seen := q.types[oid]; seen {
		return oid, nil
	}
	for _, l := range links {
		if want, ok := q.needs[l.OID]; ok && want != l.Type {
			return oid, pferr.Parsef("two objects disagree about the kind of a third")
		}
		q.needs[l.OID] = l.Type
	}
	if !q.repo.Has(oid) {
		if _, err := q.objs.write(t, data, q.objs.dir); err != nil {
			return oid, err
		}
	}
	q.types[oid] = t
	return oid, nil
}

// Read returns an object from the quarantine or, failing that, from the
// repository. ok is false when it is in neither.
func (q *Quarantine) Read(o OID) (t Type, data []byte, ok bool, err error) {
	t, data, err = q.objs.read(o)
	if errors.Is(err, errNotFound) {
		t, data, err = q.repo.objs.read(o)
	}
	if errors.Is(err, errNotFound) {
		return 0, nil, false, nil
	}
	return t, data, err == nil, err
}

// TypeOf returns the kind of an object that is in the quarantine or in the
// repository.
func (q *Quarantine) TypeOf(o OID) (Type, bool, error) {
	if t, ok := q.types[o]; ok {
		return t, true, nil
	}
	return q.repo.TypeOf(o)
}

// Count is the number of distinct objects the push sent.
func (q *Quarantine) Count() int { return len(q.types) }

// Commit checks that every object referred to by a quarantined object
// exists and has the right kind, and then moves the quarantine into the
// repository. After a successful Commit the repository still has no object
// that refers to a missing one.
func (q *Quarantine) Commit() error {
	defer q.Discard()
	for oid, want := range q.needs {
		got, ok, err := q.TypeOf(oid)
		if err != nil {
			return err
		}
		if !ok {
			return pferr.Parsef("an object refers to an object that was not sent and is not stored")
		}
		if got != want {
			return pferr.Parsef("an object refers to an object of the wrong kind")
		}
	}
	// Flush every object to disk before the marker says they are safe.
	files, _ := filepath.Glob(filepath.Join(q.objs.dir, "??", "*"))
	for _, f := range files {
		if err := syncFile(f); err != nil {
			return storageErr("accepting objects", err)
		}
	}
	if err := os.WriteFile(filepath.Join(q.objs.dir, commitMarker), nil, 0o644); err != nil {
		return storageErr("accepting objects", err)
	}
	if err := syncFile(filepath.Join(q.objs.dir, commitMarker)); err != nil {
		return storageErr("accepting objects", err)
	}
	return q.repo.moveIn(q.objs)
}

// Discard throws the quarantine away.
func (q *Quarantine) Discard() { os.RemoveAll(q.objs.dir) }
