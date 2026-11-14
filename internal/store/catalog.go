package store

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ClearedStack/plainforge/internal/pferr"
)

// CatalogSpoolDir is the directory inside a catalog root used for durable
// pending work that is not tied to a single repository.
const CatalogSpoolDir = "plainforge-spool"

// Catalog holds a set of bare <name>.git repositories inside one root
// directory on disk.
type Catalog struct {
	root  string
	mu    sync.RWMutex
	repos map[string]*Repo
}

// OpenCatalog opens every bare <name>.git repository under root, creating
// root if needed and creating any missing repository listed in initNames.
// The volume's lost+found directory and Plainforge's own spool directory are
// ignored; any other entry that is not a bare .git directory is refused.
func OpenCatalog(root string, initNames []string) (*Catalog, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, storageErr("creating the repository root", err)
	}
	c := &Catalog{root: root, repos: make(map[string]*Repo)}
	for _, raw := range initNames {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if err := CheckRepoName(name); err != nil {
			return nil, err
		}
		r, err := Open(filepath.Join(root, name+".git"))
		if err != nil {
			return nil, err
		}
		c.repos[name] = r
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, storageErr("reading the repository root", err)
	}
	for _, e := range entries {
		ename := e.Name()
		if ename == "lost+found" || ename == CatalogSpoolDir {
			continue
		}
		if !e.IsDir() || !strings.HasSuffix(ename, ".git") {
			return nil, pferr.Needsf("the repository root holds an entry that is not a bare repository")
		}
		name := strings.TrimSuffix(ename, ".git")
		if _, already := c.repos[name]; already {
			continue
		}
		r, err := Open(filepath.Join(root, ename))
		if err != nil {
			return nil, err
		}
		c.repos[name] = r
	}
	return c, nil
}

// Root returns the directory holding the bare repositories.
func (c *Catalog) Root() string { return c.root }

// SpoolDir returns the durable spool directory inside the catalog root.
func (c *Catalog) SpoolDir() string { return filepath.Join(c.root, CatalogSpoolDir) }

// Get returns the opened repository with the given name, if present.
func (c *Catalog) Get(name string) (*Repo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.repos[name]
	return r, ok
}

// Ensure opens or creates <name>.git inside the catalog root and reports
// whether it was newly added to the catalog.
func (c *Catalog) Ensure(name string) (*Repo, bool, error) {
	if err := CheckRepoName(name); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.repos[name]; ok {
		return r, false, nil
	}
	r, err := Open(filepath.Join(c.root, name+".git"))
	if err != nil {
		return nil, false, err
	}
	c.repos[name] = r
	return r, true, nil
}

// Names returns all repository names in sorted order.
func (c *Catalog) Names() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.repos))
	for name := range c.repos {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Repos returns all opened repositories sorted by name.
func (c *Catalog) Repos() []*Repo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.repos))
	for name := range c.repos {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*Repo, len(names))
	for i, name := range names {
		out[i] = c.repos[name]
	}
	return out
}
