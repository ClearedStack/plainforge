// Package forge implements Plainforge's cluster-private v1 task, branch
// proposal, and mirror control API for Bellbuoy.
package forge

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

const (
	maxLedgerBytes  = 8 << 20
	maxKeyBytes     = 256
	maxTitleBytes   = 512
	maxBodyBytes    = 32 << 10
	maxPathBytes    = 512
	maxContentBytes = 128 << 10
	maxMessageBytes = 16 << 10
	commitIdent     = "Plainforge <plainforge@clearedstack.internal> 0 +0000"
)

// Content kinds understood by SyncMirror.
const (
	ContentSource = "source"
	ContentTask   = "task"
)

var kindRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Task is one tracked failure or issue edit in a repository.
type Task struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Key         string `json:"key"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Occurrences int    `json:"occurrences"`
	Open        bool   `json:"open"`
}

// TaskDraft supplies the fields needed to open a new task.
type TaskDraft struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// FindResult is the response payload for FindOpenTask.
type FindResult struct {
	Found bool  `json:"found"`
	Task  *Task `json:"task,omitempty"`
}

// OccurrenceRequest names the open task whose occurrence count increases by one.
type OccurrenceRequest struct {
	ID string `json:"id"`
}

// Change describes a single-file replacement proposed on its own branch.
type Change struct {
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// ChangeResult reports whether ProposeChange created a new branch or matched
// an identical change already proposed on that branch.
type ChangeResult struct {
	Created bool   `json:"created"`
	Branch  string `json:"branch"`
	Commit  string `json:"commit"`
}

// MirrorRequest asks Plainforge to synchronize the named content kinds for a
// repository.
type MirrorRequest struct {
	Repo    string   `json:"repo,omitempty"`
	Content []string `json:"content"`
}

type changeRecord struct {
	Change Change `json:"change"`
	Commit string `json:"commit"`
}

type repoState struct {
	Tasks   []Task         `json:"tasks"`
	Changes []changeRecord `json:"changes"`
}

type repoLedger struct {
	mu    sync.Mutex
	path  string
	state repoState
}

// Store manages per-repository task and change ledgers on disk alongside bare
// Git repositories.
type Store struct {
	mu      sync.Mutex
	ledgers map[string]*repoLedger
}

// NewStore returns an empty ledger store that loads each repository's ledger
// on first access.
func NewStore() *Store {
	return &Store{ledgers: make(map[string]*repoLedger)}
}

func (s *Store) ledgerFor(repo *store.Repo) (*repoLedger, error) {
	dir := repo.SpoolDir()
	path := filepath.Join(dir, "forge.json")

	s.mu.Lock()
	if l, ok := s.ledgers[path]; ok {
		s.mu.Unlock()
		return l, nil
	}
	s.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, pferr.Timeoutf("the repository ledger directory could not be prepared")
	}
	loaded, err := loadLedgerFile(path)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.ledgers[path]; ok {
		return existing, nil
	}
	l := &repoLedger{path: path, state: loaded}
	s.ledgers[path] = l
	return l, nil
}

func loadLedgerFile(path string) (repoState, error) {
	var st repoState
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, pferr.Timeoutf("the repository ledger could not be read")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxLedgerBytes+1))
	if err != nil {
		return st, pferr.Timeoutf("the repository ledger could not be read")
	}
	if len(data) > maxLedgerBytes {
		return st, pferr.Parsef("the repository ledger exceeds the size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, pferr.Parsef("the repository ledger is malformed")
	}
	if _, err := dec.Token(); err != io.EOF {
		return st, pferr.Parsef("the repository ledger has trailing data")
	}
	return st, nil
}

func (l *repoLedger) saveLocked() error {
	data, err := json.Marshal(l.state)
	if err != nil {
		return pferr.Timeoutf("the repository ledger could not be encoded")
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(l.path), "forge-*.tmp")
	if err != nil {
		return pferr.Timeoutf("the repository ledger could not be written")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return pferr.Timeoutf("the repository ledger could not be written")
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return pferr.Timeoutf("the repository ledger could not be written")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return pferr.Timeoutf("the repository ledger could not be flushed")
	}
	if err := tmp.Close(); err != nil {
		return pferr.Timeoutf("the repository ledger could not be closed")
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return pferr.Timeoutf("the repository ledger could not be saved")
	}
	if d, err := os.Open(filepath.Dir(l.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func checkSingleLine(s string, max int, emptyOK bool) bool {
	if (!emptyOK && strings.TrimSpace(s) == "") || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func checkPlainText(s string, max int, emptyOK bool) bool {
	if (!emptyOK && strings.TrimSpace(s) == "") || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// CheckFilePath validates a repository-relative file path for ProposeChange.
func CheckFilePath(p string) error {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return pferr.Parsef("the file path is missing or too long")
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return pferr.Parsef("the file path must be relative")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.EqualFold(seg, ".git") {
			return pferr.Parsef("the file path contains a forbidden segment")
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return pferr.Parsef("the file path contains control characters")
			}
		}
	}
	return nil
}

// FindOpenTask returns the open task in repo matching key, if any.
func (s *Store) FindOpenTask(repo *store.Repo, key string) (Task, bool, error) {
	if !checkSingleLine(key, maxKeyBytes, false) {
		return Task{}, false, pferr.Parsef("the task key is missing or malformed")
	}
	l, err := s.ledgerFor(repo)
	if err != nil {
		return Task{}, false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range l.state.Tasks {
		if t.Open && t.Key == key {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

// OpenTask records a new open task in repo with an occurrence count of one.
func (s *Store) OpenTask(repo *store.Repo, draft TaskDraft) (Task, error) {
	if !checkSingleLine(draft.Key, maxKeyBytes, false) {
		return Task{}, pferr.Parsef("the task key is missing or malformed")
	}
	if !checkSingleLine(draft.Title, maxTitleBytes, false) {
		return Task{}, pferr.Parsef("the task title is missing or malformed")
	}
	if !checkPlainText(draft.Body, maxBodyBytes, true) {
		return Task{}, pferr.Parsef("the task body is too large or is not plain text")
	}
	l, err := s.ledgerFor(repo)
	if err != nil {
		return Task{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	num := len(l.state.Tasks) + 1
	t := Task{
		ID:          repo.Name() + "#" + strconv.Itoa(num),
		Repo:        repo.Name(),
		Key:         draft.Key,
		Title:       draft.Title,
		Body:        draft.Body,
		Occurrences: 1,
		Open:        true,
	}
	l.state.Tasks = append(l.state.Tasks, t)
	if err := l.saveLocked(); err != nil {
		l.state.Tasks = l.state.Tasks[:len(l.state.Tasks)-1]
		return Task{}, err
	}
	return t, nil
}

// AddOccurrence increments the occurrence count of an open task in repo.
func (s *Store) AddOccurrence(repo *store.Repo, taskID string) (Task, error) {
	if !checkSingleLine(taskID, maxKeyBytes, false) {
		return Task{}, pferr.Parsef("the task identifier is missing or malformed")
	}
	l, err := s.ledgerFor(repo)
	if err != nil {
		return Task{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for i := range l.state.Tasks {
		if l.state.Tasks[i].ID == taskID && l.state.Tasks[i].Open {
			l.state.Tasks[i].Occurrences++
			if err := l.saveLocked(); err != nil {
				l.state.Tasks[i].Occurrences--
				return Task{}, err
			}
			return l.state.Tasks[i], nil
		}
	}
	return Task{}, pferr.Parsef("the task to add to is not open")
}

// ProposeChange writes a single-file replacement commit onto refs/heads/<branch>
// and records the change proposal. Repeating the exact same proposal returns
// Created=false; proposing a different change on an existing branch is refused.
func (s *Store) ProposeChange(repo *store.Repo, ch Change) (ChangeResult, error) {
	if ch.Repo != "" && ch.Repo != repo.Name() {
		return ChangeResult{}, pferr.Parsef("the change repository does not match the request path")
	}
	ch.Repo = repo.Name()
	if ch.Branch == "" || ch.Branch == "main" || strings.HasPrefix(ch.Branch, "refs/") {
		return ChangeResult{}, pferr.Parsef("the change branch must be a non-main branch name")
	}
	fullRef := "refs/heads/" + ch.Branch
	if err := store.CheckRefName(fullRef); err != nil {
		return ChangeResult{}, pferr.Parsef("the change branch name is malformed")
	}
	if err := CheckFilePath(ch.Path); err != nil {
		return ChangeResult{}, err
	}
	if !checkPlainText(ch.Content, maxContentBytes, true) {
		return ChangeResult{}, pferr.Parsef("the change content is too large or is not plain text")
	}
	if !checkSingleLine(ch.Title, maxTitleBytes, false) {
		return ChangeResult{}, pferr.Parsef("the change title is missing or malformed")
	}
	if !checkPlainText(ch.Message, maxMessageBytes, false) {
		return ChangeResult{}, pferr.Parsef("the change message is missing or malformed")
	}

	repo.LockPush()
	defer repo.UnlockPush()

	l, err := s.ledgerFor(repo)
	if err != nil {
		return ChangeResult{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	existingOID, branchExists, err := repo.Ref(fullRef)
	if err != nil {
		return ChangeResult{}, err
	}
	for _, rec := range l.state.Changes {
		if rec.Change.Branch == ch.Branch {
			if rec.Change == ch && branchExists {
				return ChangeResult{Created: false, Branch: ch.Branch, Commit: existingOID.String()}, nil
			}
			return ChangeResult{}, pferr.Parsef("the branch already holds a different change")
		}
	}
	if branchExists {
		return ChangeResult{}, pferr.Parsef("the branch already holds a different change")
	}

	var parents []store.OID
	var baseTree store.OID
	if mainOID, hasMain, err := repo.Ref(store.DefaultBranch); err != nil {
		return ChangeResult{}, err
	} else if hasMain {
		kind, data, err := repo.Read(mainOID)
		if err != nil {
			return ChangeResult{}, err
		}
		if kind != store.Commit {
			return ChangeResult{}, pferr.Parsef("main does not name a commit")
		}
		ci, err := store.ParseCommit(data)
		if err != nil {
			return ChangeResult{}, err
		}
		parents = []store.OID{mainOID}
		baseTree = ci.Tree
	}

	q, err := repo.NewQuarantine()
	if err != nil {
		return ChangeResult{}, err
	}
	blobOID, err := q.Write(store.Blob, []byte(ch.Content))
	if err != nil {
		q.Discard()
		return ChangeResult{}, err
	}
	segments := strings.Split(ch.Path, "/")
	rootTreeOID, err := spliceTree(repo, q, baseTree, segments, blobOID)
	if err != nil {
		q.Discard()
		return ChangeResult{}, err
	}
	commitBytes, err := store.FormatCommit(rootTreeOID, parents, commitIdent, ch.Message)
	if err != nil {
		q.Discard()
		return ChangeResult{}, err
	}
	commitOID, err := q.Write(store.Commit, commitBytes)
	if err != nil {
		q.Discard()
		return ChangeResult{}, err
	}
	if err := q.Commit(); err != nil {
		return ChangeResult{}, err
	}

	rec := changeRecord{Change: ch, Commit: commitOID.String()}
	l.state.Changes = append(l.state.Changes, rec)
	if err := l.saveLocked(); err != nil {
		l.state.Changes = l.state.Changes[:len(l.state.Changes)-1]
		return ChangeResult{}, err
	}
	if err := repo.SetRef(fullRef, commitOID); err != nil {
		return ChangeResult{}, err
	}
	return ChangeResult{Created: true, Branch: ch.Branch, Commit: commitOID.String()}, nil
}

func spliceTree(repo *store.Repo, q *store.Quarantine, treeOID store.OID, segments []string, blobOID store.OID) (store.OID, error) {
	var entries []store.TreeEntry
	if !treeOID.IsZero() {
		kind, data, err := repo.Read(treeOID)
		if err != nil {
			return store.OID{}, err
		}
		if kind != store.Tree {
			return store.OID{}, pferr.Parsef("a directory entry did not point to a tree")
		}
		entries, err = store.ParseTree(data)
		if err != nil {
			return store.OID{}, err
		}
	}

	head := segments[0]
	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
			break
		}
	}

	if len(segments) == 1 {
		mode := "100644"
		if idx >= 0 {
			switch entries[idx].Mode {
			case "100644", "100755":
				mode = entries[idx].Mode
			default:
				return store.OID{}, pferr.Parsef("the file path conflicts with a non-file entry")
			}
			entries[idx] = store.TreeEntry{Mode: mode, Name: head, OID: blobOID}
		} else {
			entries = append(entries, store.TreeEntry{Mode: mode, Name: head, OID: blobOID})
		}
	} else {
		var subTreeOID store.OID
		if idx >= 0 {
			if entries[idx].Mode != "40000" {
				return store.OID{}, pferr.Parsef("the file path crosses a non-directory entry")
			}
			subTreeOID = entries[idx].OID
		}
		newSubOID, err := spliceTree(repo, q, subTreeOID, segments[1:], blobOID)
		if err != nil {
			return store.OID{}, err
		}
		if idx >= 0 {
			entries[idx] = store.TreeEntry{Mode: "40000", Name: head, OID: newSubOID}
		} else {
			entries = append(entries, store.TreeEntry{Mode: "40000", Name: head, OID: newSubOID})
		}
	}

	treeBytes, err := store.FormatTree(entries)
	if err != nil {
		return store.OID{}, err
	}
	return q.Write(store.Tree, treeBytes)
}

// CheckMirrorContent validates the content slice of a SyncMirror request and
// reports whether Git source commits/tags should be queued for outbound mirror
// synchronization.
func CheckMirrorContent(repoName string, req MirrorRequest) (syncSource bool, err error) {
	if req.Repo != "" && req.Repo != repoName {
		return false, pferr.Parsef("the mirror repository does not match the request path")
	}
	if len(req.Content) == 0 {
		return false, pferr.Parsef("the mirror request does not name any content")
	}
	for _, c := range req.Content {
		switch {
		case c == ContentSource:
			syncSource = true
		case c == ContentTask:
			// Tasks remain stored in Plainforge's cluster-private ledger;
			// per SPEC.md only source commits and tags mirror to GitHub.
		case kindRE.MatchString(c):
			return false, pferr.Needsf("this kind of content may not be mirrored")
		default:
			return false, pferr.Parsef("the mirror request names malformed content")
		}
	}
	return syncSource, nil
}
