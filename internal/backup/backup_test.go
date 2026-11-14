package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/plainforge/internal/age"
	"github.com/ClearedStack/plainforge/internal/forge"
	"github.com/ClearedStack/plainforge/internal/store"
)

func seedCatalogForBackup(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repos")
	cat, err := store.OpenCatalog(root, []string{"clearedstack.com", "plainforge", "empty-repo"})
	if err != nil {
		t.Fatal(err)
	}

	// Populate plainforge.git with a commit, annotated tag, and Forge v1 task ledger.
	pfRepo, _ := cat.Get("plainforge")
	q, err := pfRepo.NewQuarantine()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := q.Write(store.Blob, []byte("name: plainforge\n"))
	if err != nil {
		t.Fatal(err)
	}
	treeBytes, err := store.FormatTree([]store.TreeEntry{{Mode: "100644", Name: "app.yaml", OID: blob}})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := q.Write(store.Tree, treeBytes)
	if err != nil {
		t.Fatal(err)
	}
	commitBytes, err := store.FormatCommit(tree, nil, "Example <author@example.invalid> 1700000000 +0000", "Initial plainforge manifest\n")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := q.Write(store.Commit, commitBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := pfRepo.SetRef("refs/heads/main", commit); err != nil {
		t.Fatal(err)
	}

	// Add a Forge v1 task + branch proposal and a leftover temporary directory to
	// confirm forge.json is archived while plainforge-tmp is skipped.
	fg := forge.NewStore()
	if _, err := fg.OpenTask(pfRepo, forge.TaskDraft{
		Key:   "fp-backup-1",
		Title: "Backup task check",
		Body:  "Ensure forge.json survives backup and restore",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pfRepo.Dir(), "plainforge-tmp", "stale-temp"), []byte("ignore"), 0o644); err != nil {
		t.Fatal(err)
	}

	return root
}

func TestBackupHTTPSUploadAndExternalRestoreDrill(t *testing.T) {
	reposDir := seedCatalogForBackup(t)
	tmp := t.TempDir()

	id, err := age.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	recFile := filepath.Join(tmp, "recipient")
	idFile := filepath.Join(tmp, "identity")
	tokFile := filepath.Join(tmp, "token")
	if err := os.WriteFile(recFile, []byte("# cluster backup key\n"+id.Recipient().String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokFile, []byte("made-up-backup-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	var gotAuth string
	var gotMethod string
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		uploaded = b
		w.WriteHeader(http.StatusCreated)
	}))
	defer tlsSrv.Close()

	// Plain HTTP with a token must be refused before any network send.
	_, err = RunBackup(context.Background(), BackupOptions{
		ReposDir:      reposDir,
		ScratchDir:    filepath.Join(tmp, "scratch"),
		RecipientFile: recFile,
		DestURL:       "http://127.0.0.1:9999/backup.age",
		DestTokenFile: tokFile,
	})
	if err == nil || !strings.HasPrefix(err.Error(), "E_NEEDS_SUBTOOL:") {
		t.Fatalf("expected plain HTTP with token to be refused, got %v", err)
	}

	// Upload over HTTPS and also save local copy for restore-drill.
	outArchive := filepath.Join(tmp, "out", "nightly.tar.gz.age")
	sum, err := RunBackup(context.Background(), BackupOptions{
		ReposDir:      reposDir,
		ScratchDir:    filepath.Join(tmp, "scratch"),
		RecipientFile: recFile,
		DestURL:       tlsSrv.URL + "/plainforge/nightly.age",
		DestTokenFile: tokFile,
		OutputFile:    outArchive,
		Client:        tlsSrv.Client(),
	})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}
	if sum.Repos != 3 || sum.Refs != 1 || sum.Objects != 3 || sum.Bytes == 0 {
		t.Fatalf("unexpected backup summary: %+v", sum)
	}
	if gotMethod != http.MethodPut || gotAuth != "Bearer made-up-backup-token" {
		t.Fatalf("upload method=%q auth=%q", gotMethod, gotAuth)
	}
	if !bytes.HasPrefix(uploaded, []byte("age-encryption.org/v1\n")) {
		t.Fatalf("uploaded payload is not an age v1 file")
	}

	// Scratch directory should have no leftover temp archives.
	if entries, _ := os.ReadDir(filepath.Join(tmp, "scratch")); len(entries) != 0 {
		t.Fatalf("leftover files in scratch after backup: %v", entries)
	}

	// Run external-archive restore drill and verify all 3 repos pass Fsck.
	drillSum, err := RunRestoreDrill(DrillOptions{
		ScratchDir:   filepath.Join(tmp, "drill-scratch"),
		ArchiveFile:  outArchive,
		IdentityFile: idFile,
	})
	if err != nil {
		t.Fatalf("RunRestoreDrill (external archive): %v", err)
	}
	if drillSum.Repos != 3 || drillSum.Refs != 1 || drillSum.Objects != 3 {
		t.Fatalf("unexpected drill summary: %+v", drillSum)
	}

	// Also unpack manually to confirm forge.json survived and plainforge-tmp was omitted,
	// and verify with real git fsck --strict if git is installed.
	dec, err := age.Decrypt(bytes.NewReader(uploaded), id)
	if err != nil {
		t.Fatal(err)
	}
	manualDir := filepath.Join(tmp, "manual-restore")
	if err := ExtractArchive(dec, manualDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(manualDir, "plainforge.git", "plainforge-tmp", "stale-temp")); err == nil {
		t.Fatal("stale plainforge-tmp file should not be archived")
	}
	cat2, err := store.OpenCatalog(manualDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	pfRestored, _ := cat2.Get("plainforge")
	fg2 := forge.NewStore()
	task, found, err := fg2.FindOpenTask(pfRestored, "fp-backup-1")
	if err != nil || !found || task.Title != "Backup task check" {
		t.Fatalf("restored forge task = %+v, found=%v, err=%v", task, found, err)
	}
	if gitBin, err := exec.LookPath("git"); err == nil {
		for _, r := range cat2.Repos() {
			cmd := exec.Command(gitBin, "--git-dir", r.Dir(), "fsck", "--strict")
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git fsck %s: %v\n%s", r.Name(), err, out)
			}
		}
	}
}

func TestInClusterRestoreDrillAndFaultDetection(t *testing.T) {
	reposDir := seedCatalogForBackup(t)
	tmp := t.TempDir()

	id, _ := age.GenerateIdentity()
	recFile := filepath.Join(tmp, "recipient")
	if err := os.WriteFile(recFile, []byte(id.Recipient().String()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default monthly CronJob invocation: zero network, ephemeral drill keypair.
	sum, err := RunRestoreDrill(DrillOptions{
		ReposDir:      reposDir,
		ScratchDir:    filepath.Join(tmp, "scratch"),
		RecipientFile: recFile,
	})
	if err != nil {
		t.Fatalf("RunRestoreDrill (in-cluster mode): %v", err)
	}
	if sum.Repos != 3 || sum.Objects != 3 {
		t.Fatalf("unexpected summary: %+v", sum)
	}

	// Corrupt one loose object byte in plainforge.git and confirm both backup and
	// restore-drill detect it immediately.
	objs, err := filepath.Glob(filepath.Join(reposDir, "plainforge.git", "objects", "??", "*"))
	if err != nil || len(objs) == 0 {
		t.Fatalf("expected loose objects: %v", err)
	}
	os.Chmod(objs[0], 0o644)
	rawObj, err := os.ReadFile(objs[0])
	if err != nil {
		t.Fatal(err)
	}
	rawObj[len(rawObj)/2] ^= 0xff
	if err := os.WriteFile(objs[0], rawObj, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := RunRestoreDrill(DrillOptions{
		ReposDir:   reposDir,
		ScratchDir: filepath.Join(tmp, "scratch"),
	}); err == nil {
		t.Fatal("expected restore-drill to fail when a loose object is corrupted")
	}
}

func TestExtractArchiveRefusesTraversalAndSymlinks(t *testing.T) {
	cases := []struct {
		name string
		hdr  *tar.Header
	}{
		{"path traversal", &tar.Header{Typeflag: tar.TypeReg, Name: "../escape.txt", Size: 0, Mode: 0o644}},
		{"outside bare repo", &tar.Header{Typeflag: tar.TypeReg, Name: "plain.txt", Size: 0, Mode: 0o644}},
		{"symlink entry", &tar.Header{Typeflag: tar.TypeSymlink, Name: "demo.git/HEAD", Linkname: "/etc/passwd"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			gw := gzip.NewWriter(&buf)
			tw := tar.NewWriter(gw)
			if err := tw.WriteHeader(c.hdr); err != nil {
				t.Fatal(err)
			}
			tw.Close()
			gw.Close()

			if err := ExtractArchive(&buf, t.TempDir()); err == nil {
				t.Fatalf("expected %s to be refused", c.name)
			}
		})
	}
}
