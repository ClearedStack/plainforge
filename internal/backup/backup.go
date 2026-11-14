// Package backup archives, age-encrypts, uploads, and restore-tests Plainforge
// bare Git repository catalogs without shelling out to external tools.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/ClearedStack/plainforge/internal/age"
	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

const (
	maxArchiveBytes = 512 << 20
	maxArchiveFiles = 100000
	uploadTimeout   = 5 * time.Minute
)

// Summary reports the repositories, refs, and objects verified during a backup
// or restore drill.
type Summary struct {
	Repos   int
	Refs    int
	Objects int
	Bytes   int64
}

// BackupOptions configures a backup run.
type BackupOptions struct {
	ReposDir      string
	ScratchDir    string
	RecipientFile string
	DestURL       string
	DestTokenFile string
	OutputFile    string
	Client        *http.Client
}

// DrillOptions configures an isolated restore drill.
type DrillOptions struct {
	ReposDir      string
	ScratchDir    string
	RecipientFile string
	ArchiveFile   string
	IdentityFile  string
}

// ArchiveCatalog verifies every bare repository under reposDir in read-only
// mode and writes a deterministic tar.gz snapshot to dst.
func ArchiveCatalog(reposDir string, dst io.Writer) (Summary, error) {
	var sum Summary
	cat, err := store.OpenCatalogReadOnly(reposDir)
	if err != nil {
		return sum, err
	}
	repos := cat.Repos()
	for _, r := range repos {
		rep, err := r.Fsck()
		if err != nil {
			return sum, err
		}
		sum.Repos++
		sum.Refs += rep.Refs
		sum.Objects += rep.Objects
	}

	gw := gzip.NewWriter(dst)
	gw.ModTime = time.Unix(0, 0).UTC()
	tw := tar.NewWriter(gw)

	epoch := time.Unix(0, 0).UTC()
	writeDir := func(rel string) error {
		hdr := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     strings.TrimSuffix(rel, "/") + "/",
			Mode:     0o755,
			ModTime:  epoch,
			Format:   tar.FormatPAX,
		}
		return tw.WriteHeader(hdr)
	}

	for _, r := range repos {
		repoBase := r.Name() + ".git"
		for _, reqDir := range []string{
			repoBase,
			repoBase + "/objects",
			repoBase + "/refs",
			repoBase + "/refs/heads",
			repoBase + "/refs/tags",
		} {
			if err := writeDir(reqDir); err != nil {
				return sum, pferr.Parsef("writing archive directory entry failed")
			}
		}

		files, dirs, err := collectRepoEntries(r.Dir(), repoBase)
		if err != nil {
			return sum, err
		}
		for _, d := range dirs {
			if err := writeDir(d); err != nil {
				return sum, pferr.Parsef("writing archive directory entry failed")
			}
		}
		for _, f := range files {
			if err := writeRegularFile(tw, f.diskPath, f.arcPath, epoch); err != nil {
				return sum, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return sum, pferr.Parsef("closing tar archive failed")
	}
	if err := gw.Close(); err != nil {
		return sum, pferr.Parsef("closing compressed archive failed")
	}
	return sum, nil
}

type archiveEntry struct {
	diskPath string
	arcPath  string
}

func collectRepoEntries(repoDir, repoBase string) (files []archiveEntry, extraDirs []string, err error) {
	seenReq := map[string]bool{
		repoBase:                 true,
		repoBase + "/objects":    true,
		repoBase + "/refs":       true,
		repoBase + "/refs/heads": true,
		repoBase + "/refs/tags":  true,
	}
	walkErr := filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return pferr.Parsef("walking repository for backup failed")
		}
		relOS, err := filepath.Rel(repoDir, p)
		if err != nil || relOS == "." {
			return err
		}
		rel := filepath.ToSlash(relOS)
		if rel == "plainforge-tmp" || strings.HasPrefix(rel, "plainforge-tmp/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Inside plainforge-spool, keep only durable forge.json task/change state;
		// skip transient outbound notify/mirror retry queues.
		if strings.HasPrefix(rel, "plainforge-spool/") && rel != "plainforge-spool/forge.json" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(rel, ".lock") {
			return nil
		}

		arc := repoBase + "/" + rel
		if d.IsDir() {
			if rel == "plainforge-spool" {
				if _, stErr := os.Stat(filepath.Join(repoDir, "plainforge-spool", "forge.json")); stErr != nil {
					return filepath.SkipDir
				}
			}
			if !seenReq[arc] {
				extraDirs = append(extraDirs, arc)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return pferr.Needsf("a repository holds a non-regular file that Plainforge will not archive")
		}
		files = append(files, archiveEntry{diskPath: p, arcPath: arc})
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	sort.Strings(extraDirs)
	sort.Slice(files, func(i, j int) bool { return files[i].arcPath < files[j].arcPath })
	return files, extraDirs, nil
}

func writeRegularFile(tw *tar.Writer, diskPath, arcPath string, modTime time.Time) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return pferr.Parsef("opening repository file for backup failed")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return pferr.Parsef("stating repository file for backup failed")
	}
	mode := int64(0o644)
	if strings.Contains(arcPath, "/objects/") {
		mode = 0o444
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     arcPath,
		Size:     st.Size(),
		Mode:     mode,
		ModTime:  modTime,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return pferr.Parsef("writing archive file header failed")
	}
	if _, err := io.CopyN(tw, f, st.Size()); err != nil {
		return pferr.Parsef("copying repository file into archive failed")
	}
	return nil
}

// ExtractArchive safely unpacks a Plainforge tar.gz archive into dstDir,
// refusing path traversal, symlinks, non-regular entries, and oversized inputs.
func ExtractArchive(src io.Reader, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return pferr.Parsef("creating restore directory failed")
	}
	gr, err := gzip.NewReader(src)
	if err != nil {
		return pferr.Parsef("opening compressed backup stream failed")
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	var totalBytes int64
	var fileCount int

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pferr.Parsef("reading tar entry from backup failed")
		}
		fileCount++
		if fileCount > maxArchiveFiles {
			return pferr.Needsf("the backup archive holds more entries than Plainforge unpacks")
		}

		cleanRel, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, filepath.FromSlash(cleanRel))
		if !isWithinDir(dstDir, target) {
			return pferr.Parsef("an archive entry escaped the restore directory")
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return pferr.Parsef("creating restored directory failed")
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 || totalBytes+hdr.Size > maxArchiveBytes {
				return pferr.Needsf("the backup archive is larger than the restore scratch limit")
			}
			totalBytes += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return pferr.Parsef("creating parent directory for restored file failed")
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
			if err != nil {
				return pferr.Parsef("creating restored file failed")
			}
			_, copyErr := io.Copy(out, io.LimitReader(tr, hdr.Size+1))
			closeErr := out.Close()
			if copyErr != nil || closeErr != nil {
				return pferr.Parsef("writing restored file failed")
			}
			if strings.Contains(cleanRel, "/objects/") {
				_ = os.Chmod(target, 0o444)
			}
		default:
			return pferr.Needsf("the backup archive contains a non-regular entry")
		}
	}
	return nil
}

func cleanArchivePath(raw string) (string, error) {
	trimmed := strings.TrimSuffix(raw, "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.ContainsRune(trimmed, '\\') || strings.IndexByte(trimmed, 0) >= 0 {
		return "", pferr.Parsef("an archive entry had an unsafe path")
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "" || part == "." || part == ".." {
			return "", pferr.Parsef("an archive entry had an unsafe path component")
		}
	}
	cleaned := path.Clean(trimmed)
	top, _, _ := strings.Cut(cleaned, "/")
	if !strings.HasSuffix(top, ".git") {
		return "", pferr.Parsef("an archive entry was not inside a bare repository directory")
	}
	if err := store.CheckRepoName(strings.TrimSuffix(top, ".git")); err != nil {
		return "", err
	}
	return cleaned, nil
}

func isWithinDir(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// RunBackup snapshots and age-encrypts every repository in opts.ReposDir via
// opts.ScratchDir, writing to opts.OutputFile and/or uploading via HTTP PUT to
// opts.DestURL.
func RunBackup(ctx context.Context, opts BackupOptions) (Summary, error) {
	var sum Summary
	if opts.ReposDir == "" || opts.ScratchDir == "" || opts.RecipientFile == "" {
		return sum, pferr.Parsef("repos, scratch, and recipient-file are required")
	}
	if opts.DestURL == "" && opts.OutputFile == "" {
		return sum, pferr.Needsf("the backup destination URL is not configured")
	}

	recBytes, err := os.ReadFile(opts.RecipientFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sum, pferr.Needsf("the backup recipient key is not configured")
		}
		return sum, pferr.Parsef("the backup recipient key file could not be read")
	}
	recipient, err := age.ParseRecipientFile(recBytes)
	if err != nil {
		return sum, err
	}

	token, err := readOptionalToken(opts.DestTokenFile)
	if err != nil {
		return sum, err
	}
	if opts.DestURL != "" {
		if err := validateDestURL(opts.DestURL, token != ""); err != nil {
			return sum, err
		}
	}

	if err := os.MkdirAll(opts.ScratchDir, 0o700); err != nil {
		return sum, pferr.Parsef("the scratch directory could not be prepared")
	}
	tmpFile, err := os.CreateTemp(opts.ScratchDir, "backup-*.tar.gz.age")
	if err != nil {
		return sum, pferr.Parsef("a temporary backup file could not be created")
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	encWriter, err := age.Encrypt(tmpFile, recipient)
	if err != nil {
		tmpFile.Close()
		return sum, err
	}
	sum, err = ArchiveCatalog(opts.ReposDir, encWriter)
	if err != nil {
		encWriter.Close()
		tmpFile.Close()
		return sum, err
	}
	if err := encWriter.Close(); err != nil {
		tmpFile.Close()
		return sum, pferr.Public(err, "finalizing the encrypted archive failed")
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return sum, pferr.Parsef("flushing the encrypted archive failed")
	}
	st, err := tmpFile.Stat()
	if err != nil {
		tmpFile.Close()
		return sum, pferr.Parsef("stating the encrypted archive failed")
	}
	sum.Bytes = st.Size()
	if err := tmpFile.Close(); err != nil {
		return sum, pferr.Parsef("closing the encrypted archive failed")
	}

	if opts.DestURL != "" {
		if err := uploadArchive(ctx, opts.Client, opts.DestURL, token, tmpPath, sum.Bytes); err != nil {
			return sum, err
		}
	}
	if opts.OutputFile != "" {
		if err := copyFileAtomic(tmpPath, opts.OutputFile); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

// RunRestoreDrill verifies end-to-end backup recoverability inside an isolated
// directory under opts.ScratchDir with zero network access, running Repo.Fsck
// across every unpacked repository.
func RunRestoreDrill(opts DrillOptions) (Summary, error) {
	var sum Summary
	if opts.ScratchDir == "" {
		return sum, pferr.Parsef("scratch directory is required")
	}
	if opts.ArchiveFile != "" {
		if opts.IdentityFile == "" {
			return sum, pferr.Parsef("identity-file is required when archive is specified")
		}
	} else if opts.ReposDir == "" {
		return sum, pferr.Parsef("either repos or archive must be specified")
	}

	if opts.RecipientFile != "" {
		if recBytes, err := os.ReadFile(opts.RecipientFile); err == nil {
			if _, err := age.ParseRecipientFile(recBytes); err != nil {
				return sum, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return sum, pferr.Parsef("the backup recipient key file could not be read")
		}
	}

	if err := os.MkdirAll(opts.ScratchDir, 0o700); err != nil {
		return sum, pferr.Parsef("the scratch directory could not be prepared")
	}
	workDir, err := os.MkdirTemp(opts.ScratchDir, "drill-*")
	if err != nil {
		return sum, pferr.Parsef("the restore drill workspace could not be created")
	}
	defer os.RemoveAll(workDir)

	var archivePath string
	var identity age.Identity
	var wantRepoNames []string

	if opts.ArchiveFile != "" {
		idBytes, err := os.ReadFile(opts.IdentityFile)
		if err != nil {
			return sum, pferr.Parsef("the age identity file could not be read")
		}
		identity, err = age.ParseIdentityFile(idBytes)
		if err != nil {
			return sum, err
		}
		archivePath = opts.ArchiveFile
	} else {
		srcCat, err := store.OpenCatalogReadOnly(opts.ReposDir)
		if err != nil {
			return sum, err
		}
		wantRepoNames = srcCat.Names()

		identity, err = age.GenerateIdentity()
		if err != nil {
			return sum, err
		}
		archivePath = filepath.Join(workDir, "drill.tar.gz.age")
		af, err := os.OpenFile(archivePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			return sum, pferr.Parsef("creating temporary drill archive failed")
		}
		enc, err := age.Encrypt(af, identity.Recipient())
		if err != nil {
			af.Close()
			return sum, err
		}
		if _, err := ArchiveCatalog(opts.ReposDir, enc); err != nil {
			enc.Close()
			af.Close()
			return sum, err
		}
		if err := enc.Close(); err != nil {
			af.Close()
			return sum, pferr.Public(err, "finalizing drill archive failed")
		}
		if err := af.Close(); err != nil {
			return sum, pferr.Parsef("closing drill archive failed")
		}
	}

	af, err := os.Open(archivePath)
	if err != nil {
		return sum, pferr.Parsef("opening encrypted archive for drill failed")
	}
	defer af.Close()
	st, err := af.Stat()
	if err != nil {
		return sum, pferr.Parsef("stating encrypted archive for drill failed")
	}
	sum.Bytes = st.Size()

	decReader, err := age.Decrypt(af, identity)
	if err != nil {
		return sum, err
	}
	restoredDir := filepath.Join(workDir, "restored")
	if err := ExtractArchive(decReader, restoredDir); err != nil {
		return sum, err
	}
	// Drain EOF so any trailing-byte authentication check runs.
	if n, err := decReader.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		if err != nil {
			return sum, pferr.Public(err, "encrypted archive stream verification failed")
		}
		return sum, pferr.Parsef("unexpected trailing plaintext in backup archive")
	}

	cat, err := store.OpenCatalog(restoredDir, nil)
	if err != nil {
		return sum, err
	}
	if wantRepoNames != nil && !reflect.DeepEqual(cat.Names(), wantRepoNames) {
		return sum, pferr.Parsef("restored repository set did not match source catalog")
	}
	for _, r := range cat.Repos() {
		rep, err := r.Fsck()
		if err != nil {
			return sum, err
		}
		sum.Repos++
		sum.Refs += rep.Refs
		sum.Objects += rep.Objects
	}
	return sum, nil
}

func readOptionalToken(tokenFile string) (string, error) {
	if tokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", pferr.Parsef("the backup credential file could not be read")
	}
	tok := strings.TrimSpace(string(b))
	if strings.ContainsAny(tok, "\r\n\x00") {
		return "", pferr.Parsef("the backup credential contained forbidden bytes")
	}
	return tok, nil
}

func validateDestURL(rawURL string, hasCred bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return pferr.Parsef("the backup destination URL is malformed")
	}
	if u.User != nil {
		return pferr.Needsf("credentials in the backup URL are not accepted; use -dest-token-file")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if hasCred {
			return pferr.Needsf("refusing to send backup credentials over plain HTTP")
		}
		host := u.Hostname()
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return nil
		}
		return pferr.Needsf("refusing to upload backups over plain HTTP to a non-loopback host")
	default:
		return pferr.Needsf("the backup destination URL must use HTTPS")
	}
}

func uploadArchive(ctx context.Context, client *http.Client, destURL, token, archivePath string, size int64) error {
	if client == nil {
		client = &http.Client{
			Timeout: uploadTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return pferr.Parsef("opening staged backup archive failed")
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, destURL, f)
	if err != nil {
		return pferr.Parsef("building backup upload request failed")
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return pferr.Timeoutf("uploading the backup archive timed out or failed")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout:
		return pferr.Timeoutf("the backup destination is temporarily unavailable")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return pferr.Needsf("the backup destination rejected the upload credentials")
	default:
		return pferr.Parsef("the backup destination rejected the archive upload")
	}
}

func copyFileAtomic(srcPath, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return pferr.Parsef("creating output directory failed")
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return pferr.Parsef("opening staged archive failed")
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".plainforge-backup-*")
	if err != nil {
		return pferr.Parsef("creating output file failed")
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return pferr.Parsef("writing output file failed")
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return pferr.Parsef("setting output file permissions failed")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return pferr.Parsef("flushing output file failed")
	}
	if err := tmp.Close(); err != nil {
		return pferr.Parsef("closing output file failed")
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return pferr.Parsef("moving output file into place failed")
	}
	return nil
}
