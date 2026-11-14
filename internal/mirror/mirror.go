// Package mirror pushes source commits on main and tags outward over Git's
// smart HTTP protocol.
package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/plainforge/internal/pack"
	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/pkt"
	"github.com/ClearedStack/plainforge/internal/store"
)

// DefaultTimeout bounds one outbound mirror exchange.
const DefaultTimeout = 30 * time.Second

// agent identifies Plainforge in smart HTTP capabilities.
const agent = "agent=plainforge/v0.1.0-draft"

// maxMirrorToken bounds the credential file read from disk.
const maxMirrorToken = 4096

// IsMirrorRef reports whether ref is mirrored outward. Only refs/heads/main
// and refs/tags/* are mirrored; feature branches and any other namespace stay
// inside the cluster.
func IsMirrorRef(ref string) bool {
	if store.CheckRefName(ref) != nil {
		return false
	}
	return ref == store.DefaultBranch || strings.HasPrefix(ref, "refs/tags/")
}

// Pusher brings one repository's public mirror up to date.
type Pusher interface {
	Sync(ctx context.Context, repo *store.Repo) error
}

// HTTP pushes refs/heads/main and refs/tags/* to <BaseURL>/<repo>.git over
// smart HTTP.
type HTTP struct {
	BaseURL           string
	TokenFile         string
	AllowInsecureHTTP bool
	Timeout           time.Duration
	client            *http.Client
}

// NewHTTP returns a mirror client for the given base URL, such as
// "https://github.com/ClearedStack".
func NewHTTP(baseURL string) *HTTP {
	return &HTTP{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: DefaultTimeout,
	}
}

func (h *HTTP) httpClient() *http.Client {
	if h.client != nil {
		return h.client
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (h *HTTP) authHeader() (string, error) {
	if h.TokenFile == "" {
		return "", nil
	}
	if !strings.HasPrefix(h.BaseURL, "https://") && !h.AllowInsecureHTTP {
		return "", pferr.Parsef("the mirror credential requires an https address")
	}
	f, err := os.Open(h.TokenFile)
	if err != nil {
		return "", pferr.Parsef("the mirror token could not be read")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMirrorToken+16))
	if err != nil {
		return "", pferr.Parsef("the mirror token could not be read")
	}
	tok := strings.TrimRight(string(b), "\r\n")
	if len(tok) == 0 || len(tok) > maxMirrorToken || strings.ContainsAny(tok, " \t\r\n") {
		return "", pferr.Parsef("the mirror token is malformed")
	}
	enc := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	return "Basic " + enc, nil
}

func classifyStatus(status int) error {
	switch {
	case status >= 200 && status <= 299:
		return nil
	case status == http.StatusTooManyRequests || (status >= 500 && status <= 599):
		return pferr.Timeoutf("the mirror answered with status %d", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return pferr.Parsef("the mirror refused the credential")
	case status == http.StatusNotFound:
		return pferr.Needsf("the mirror has no repository for this name")
	default:
		return pferr.Needsf("the mirror answered with status %d", status)
	}
}

// ParseAdvertisement parses a smart HTTP git-receive-pack ref advertisement.
func ParseAdvertisement(r io.Reader) (refs map[string]store.OID, caps []string, err error) {
	pr := pkt.NewReader(r)
	line, flush, err := pr.Next()
	if err != nil {
		return nil, nil, pferr.Parsef("the mirror ref advertisement could not be read")
	}
	if flush || strings.TrimSuffix(string(line), "\n") != "# service=git-receive-pack" {
		return nil, nil, pferr.Parsef("the mirror did not answer with git-receive-pack")
	}
	_, flush, err = pr.Next()
	if err != nil || !flush {
		return nil, nil, pferr.Parsef("the mirror ref advertisement had no service flush")
	}

	refs = make(map[string]store.OID)
	for first := true; ; first = false {
		line, flush, err := pr.Next()
		if err != nil {
			return nil, nil, pferr.Parsef("the mirror ref advertisement ended unexpectedly")
		}
		if flush {
			break
		}
		text := string(line)
		if first {
			before, capText, hasCaps := strings.Cut(text, "\x00")
			if !hasCaps {
				return nil, nil, pferr.Parsef("the mirror advertised no capabilities")
			}
			caps = strings.Fields(strings.TrimSuffix(capText, "\n"))
			text = before
		} else {
			if strings.IndexByte(text, 0) >= 0 {
				return nil, nil, pferr.Parsef("the mirror sent capabilities after the first line")
			}
			text = strings.TrimSuffix(text, "\n")
		}
		oidText, name, ok := strings.Cut(text, " ")
		if !ok {
			return nil, nil, pferr.Parsef("the mirror advertised a malformed ref line")
		}
		oid, err := store.ParseOID(oidText)
		if err != nil {
			return nil, nil, pferr.Parsef("the mirror advertised a malformed object name")
		}
		if first && oid.IsZero() && name == "capabilities^{}" {
			continue
		}
		if strings.HasSuffix(name, "^{}") {
			continue
		}
		refs[name] = oid
	}
	return refs, caps, nil
}

// ParseReportStatus reads a git-receive-pack report-status response and
// checks that unpack and every expected ref succeeded.
func ParseReportStatus(r io.Reader, expectedRefs []string) error {
	pr := pkt.NewReader(r)
	var lines []string
	var sideband bytes.Buffer
	usedSideband := false

	for {
		payload, flush, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return pferr.Parsef("the mirror status report was malformed")
		}
		if flush {
			break
		}
		switch payload[0] {
		case 1:
			usedSideband = true
			sideband.Write(payload[1:])
		case 2:
			usedSideband = true
		case 3:
			return pferr.Needsf("the mirror aborted the push")
		default:
			lines = append(lines, strings.TrimSuffix(string(payload), "\n"))
		}
	}
	if usedSideband {
		inner := pkt.NewReader(&sideband)
		for {
			payload, flush, err := inner.Next()
			if err == io.EOF || flush {
				break
			}
			if err != nil {
				return pferr.Parsef("the mirror sideband status report was malformed")
			}
			lines = append(lines, strings.TrimSuffix(string(payload), "\n"))
		}
	}
	if len(lines) == 0 {
		return pferr.Parsef("the mirror returned an empty status report")
	}
	if lines[0] != "unpack ok" {
		return pferr.Needsf("the mirror did not accept the pack")
	}
	okRefs := make(map[string]bool, len(lines)-1)
	for _, l := range lines[1:] {
		if ref, ok := strings.CutPrefix(l, "ok "); ok {
			okRefs[ref] = true
			continue
		}
		if strings.HasPrefix(l, "ng ") {
			return pferr.Needsf("the mirror refused a ref update")
		}
		return pferr.Parsef("the mirror returned an unexpected status line")
	}
	for _, ref := range expectedRefs {
		if !okRefs[ref] {
			return pferr.Parsef("the mirror did not report every pushed ref")
		}
	}
	return nil
}

type refUpdate struct {
	old  store.OID
	new  store.OID
	name string
}

// Sync pushes any new source commits on refs/heads/main and new tags in repo
// to <BaseURL>/<repo.Name()>.git.
func (h *HTTP) Sync(ctx context.Context, repo *store.Repo) error {
	if h.BaseURL == "" {
		return pferr.Parsef("the mirror address is empty")
	}
	localRefs, err := repo.Refs()
	if err != nil {
		return err
	}
	var candidates []store.Ref
	for _, r := range localRefs {
		if !IsMirrorRef(r.Name) {
			continue
		}
		kind, ok, err := repo.TypeOf(r.OID)
		if err != nil {
			return err
		}
		if !ok {
			return pferr.Parsef("the repository is missing an object it needs")
		}
		if r.Name == store.DefaultBranch && kind != store.Commit {
			return pferr.Parsef("main does not name a commit")
		}
		if strings.HasPrefix(r.Name, "refs/tags/") && kind != store.Commit && kind != store.Tag {
			return pferr.Needsf("a tag names an object that cannot be mirrored")
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return nil
	}

	auth, err := h.authHeader()
	if err != nil {
		return err
	}
	repoBase := strings.TrimRight(h.BaseURL, "/") + "/" + repo.Name() + ".git"

	advReq, err := http.NewRequestWithContext(ctx, http.MethodGet, repoBase+"/info/refs?service=git-receive-pack", nil)
	if err != nil {
		return pferr.Parsef("the mirror address is not a usable URL")
	}
	advReq.Header.Set("User-Agent", "plainforge")
	if auth != "" {
		advReq.Header.Set("Authorization", auth)
	}

	client := h.httpClient()
	advResp, err := client.Do(advReq)
	if err != nil {
		return pferr.Timeoutf("the mirror was not reachable in time")
	}
	defer advResp.Body.Close()
	if err := classifyStatus(advResp.StatusCode); err != nil {
		io.Copy(io.Discard, io.LimitReader(advResp.Body, 64<<10))
		return err
	}
	remoteRefs, caps, err := ParseAdvertisement(io.LimitReader(advResp.Body, 4<<20))
	if err != nil {
		return err
	}
	hasReport := false
	for _, c := range caps {
		if c == "report-status" {
			hasReport = true
			break
		}
	}
	if !hasReport {
		return pferr.Needsf("the mirror does not offer report-status")
	}

	var updates []refUpdate
	for _, r := range candidates {
		old := remoteRefs[r.Name]
		if old == r.OID {
			continue
		}
		if !old.IsZero() {
			if strings.HasPrefix(r.Name, "refs/tags/") {
				return pferr.Needsf("the mirror holds a different target for an existing tag")
			}
			if !repo.Has(old) {
				return pferr.Needsf("the mirror holds a commit on main that Plainforge does not have")
			}
			ff, err := repo.IsAncestor(old, r.OID)
			if err != nil {
				return err
			}
			if !ff {
				return pferr.Needsf("the mirror is ahead of or diverged from main")
			}
		}
		updates = append(updates, refUpdate{old: old, new: r.OID, name: r.Name})
	}
	if len(updates) == 0 {
		return nil
	}

	skip := make(map[store.OID]struct{})
	var haveRoots []store.OID
	for _, oid := range remoteRefs {
		if !oid.IsZero() && repo.Has(oid) {
			haveRoots = append(haveRoots, oid)
		}
	}
	if _, err := repo.Walk(haveRoots, skip); err != nil {
		return err
	}
	wantRoots := make([]store.OID, len(updates))
	expectedRefs := make([]string, len(updates))
	for i, u := range updates {
		wantRoots[i] = u.new
		expectedRefs[i] = u.name
	}
	toSend, err := repo.Walk(wantRoots, skip)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() {
		var werr error
		for i, u := range updates {
			line := u.old.String() + " " + u.new.String() + " " + u.name
			if i == 0 {
				line += "\x00report-status " + agent
			}
			if werr = pkt.WriteString(pw, line+"\n"); werr != nil {
				pw.CloseWithError(werr)
				return
			}
		}
		if werr = pkt.Flush(pw); werr != nil {
			pw.CloseWithError(werr)
			return
		}
		if werr = pack.Write(pw, toSend, repo.Read); werr != nil {
			pw.CloseWithError(werr)
			return
		}
		pw.Close()
	}()

	pushReq, err := http.NewRequestWithContext(ctx, http.MethodPost, repoBase+"/git-receive-pack", pr)
	if err != nil {
		pr.Close()
		return pferr.Parsef("the mirror address is not a usable URL")
	}
	pushReq.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	pushReq.Header.Set("User-Agent", "plainforge")
	if auth != "" {
		pushReq.Header.Set("Authorization", auth)
	}

	pushResp, err := client.Do(pushReq)
	if err != nil {
		return pferr.Timeoutf("the mirror was not reachable in time")
	}
	defer pushResp.Body.Close()
	if err := classifyStatus(pushResp.StatusCode); err != nil {
		io.Copy(io.Discard, io.LimitReader(pushResp.Body, 64<<10))
		return err
	}
	return ParseReportStatus(io.LimitReader(pushResp.Body, 1<<20), expectedRefs)
}
