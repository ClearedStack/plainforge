// Package server serves bare Git repositories over Git's smart HTTP protocol.
package server

import (
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ClearedStack/plainforge/internal/mirror"
	"github.com/ClearedStack/plainforge/internal/notify"
	"github.com/ClearedStack/plainforge/internal/pack"
	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/pkt"
	"github.com/ClearedStack/plainforge/internal/store"
)

// Limits. None of these numbers comes from the specification; see the
// README.
const (
	// RequestTimeout is how long one request may take to arrive and one
	// answer may take to leave.
	RequestTimeout = 2 * time.Minute
	// MaxFetchRequest bounds the body of a fetch request.
	MaxFetchRequest = 4 << 20
	// MaxPush bounds the body of a push.
	MaxPush = 256 << 20
)

// Server serves one repository (Repo) or a directory of repositories
// (Catalog) over Git's smart HTTP protocol.
type Server struct {
	Repo    *store.Repo
	Catalog *store.Catalog
	// Notify announces accepted pushes to Keelworks. Nil turns announcements off.
	Notify notify.Sender
	// Mirror schedules outbound source mirroring on main and tag updates.
	// Nil turns mirroring off.
	Mirror mirror.Enqueuer
	// Log receives one line per accepted or refused ref update.
	Log *log.Logger
	// Timeout replaces RequestTimeout when it is not zero.
	Timeout time.Duration
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(format, a...)
	}
}

func (s *Server) lookupRepo(name string) (*store.Repo, bool) {
	if s.Catalog != nil {
		if r, ok := s.Catalog.Get(name); ok {
			return r, true
		}
	}
	if s.Repo != nil && s.Repo.Name() == name {
		return s.Repo, true
	}
	return nil, false
}

// fail answers a request with a public error as plain text.
func fail(w http.ResponseWriter, err error) {
	pe := pferr.Public(err, "the request could not be handled")
	status := http.StatusBadRequest
	switch pe.Code {
	case pferr.Timeout:
		status = http.StatusGatewayTimeout
	case pferr.NeedsSubtool:
		status = http.StatusUnprocessableEntity
	}
	failStatus(w, status, pe)
}

func failStatus(w http.ResponseWriter, status int, pe *pferr.Error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	io.WriteString(w, pe.Error()+"\n")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	timeout := RequestTimeout
	if s.Timeout != 0 {
		timeout = s.Timeout
	}
	deadline := time.Now().Add(timeout)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline.Add(timeout))

	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			failStatus(w, http.StatusMethodNotAllowed, pferr.Needsf("this page does not answer that HTTP method"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "{\"status\":\"ok\"}\n")
		return
	}

	trimmed, ok := strings.CutPrefix(r.URL.Path, "/")
	if !ok {
		failStatus(w, http.StatusNotFound, pferr.Needsf("there is no such repository or page"))
		return
	}
	seg, rest, hasSlash := strings.Cut(trimmed, "/")
	repoName, hasGit := strings.CutSuffix(seg, ".git")
	if !hasSlash || !hasGit {
		failStatus(w, http.StatusNotFound, pferr.Needsf("there is no such repository or page"))
		return
	}
	repo, ok := s.lookupRepo(repoName)
	if !ok {
		failStatus(w, http.StatusNotFound, pferr.Needsf("there is no such repository or page"))
		return
	}

	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		s.infoRefs(w, r, repo)
	case rest == serviceUpload && r.Method == http.MethodPost:
		s.uploadPack(w, r, repo)
	case rest == serviceReceive && r.Method == http.MethodPost:
		s.receivePack(w, r, repo)
	case rest == "info/refs" || rest == serviceUpload || rest == serviceReceive:
		failStatus(w, http.StatusMethodNotAllowed, pferr.Needsf("this page does not answer that HTTP method"))
	default:
		failStatus(w, http.StatusNotFound, pferr.Needsf("there is no such repository or page"))
	}
}

// limited reads at most max bytes and then fails with a public error.
type limited struct {
	r   io.Reader
	max int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.max <= 0 {
		return 0, pferr.Needsf("the request is larger than Plainforge accepts")
	}
	if int64(len(p)) > l.max {
		p = p[:l.max]
	}
	n, err := l.r.Read(p)
	l.max -= int64(n)
	return n, err
}

// body returns the request body, decompressed when the client compressed
// it, and bounded both before and after decompression.
func body(r *http.Request, service string, max int64) (io.Reader, error) {
	if ct := r.Header.Get("Content-Type"); ct != "application/x-"+service+"-request" {
		return nil, pferr.Parsef("the request does not have the content type of a Git request")
	}
	var in io.Reader = &limited{r: r.Body, max: max + 1}
	switch r.Header.Get("Content-Encoding") {
	case "":
	case "gzip":
		zr, err := gzip.NewReader(in)
		if err != nil {
			return nil, pferr.Public(err, "the request body is not valid gzip data")
		}
		in = zr
	default:
		return nil, pferr.Needsf("the request body uses an unknown content encoding")
	}
	return &limited{r: in, max: max + 1}, nil
}

func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	service := r.URL.Query().Get("service")
	if service != serviceUpload && service != serviceReceive {
		fail(w, pferr.Needsf("only the smart HTTP protocol is supported"))
		return
	}
	head, err := repo.HeadTarget()
	if err != nil {
		fail(w, err)
		return
	}
	refs, err := repo.Refs()
	if err != nil {
		fail(w, err)
		return
	}
	var peeled []Peeled
	if service == serviceUpload {
		if peeled, err = peel(repo, refs); err != nil {
			fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(Advertise(service, head, refs, peeled))
}

// peel follows annotated tags to the object behind them.
func peel(repo *store.Repo, refs []store.Ref) ([]Peeled, error) {
	var out []Peeled
	for _, r := range refs {
		oid := r.OID
		for depth := 0; ; depth++ {
			t, data, err := repo.Read(oid)
			if err != nil {
				return nil, err
			}
			if t != store.Tag {
				break
			}
			if depth > 16 {
				return nil, pferr.Needsf("a tag is nested more deeply than Plainforge follows")
			}
			link, err := store.ParseTag(data)
			if err != nil {
				return nil, err
			}
			oid = link.OID
		}
		if oid != r.OID {
			out = append(out, Peeled{Ref: r.Name, OID: oid})
		}
	}
	return out, nil
}

func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	in, err := body(r, serviceUpload, MaxFetchRequest)
	if err != nil {
		fail(w, err)
		return
	}
	req, err := ParseUploadRequest(in)
	if err != nil {
		fail(w, err)
		return
	}
	refs, err := repo.Refs()
	if err != nil {
		fail(w, err)
		return
	}
	tips := map[store.OID]bool{}
	for _, ref := range refs {
		tips[ref.OID] = true
	}
	for _, want := range req.Wants {
		if !tips[want] {
			fail(w, pferr.Needsf("a wanted object is not the tip of a branch or a tag"))
			return
		}
	}
	if len(req.Haves) > 0 && !hasCap(req.Caps, "multi_ack_detailed") {
		fail(w, pferr.Needsf("a fetch that names what it has must use multi_ack_detailed"))
		return
	}
	var common []store.OID
	seen := map[store.OID]bool{}
	for _, have := range req.Haves {
		if !seen[have] && repo.Has(have) {
			common = append(common, have)
		}
		seen[have] = true
	}

	var send []store.OID
	if req.Done {
		skip := map[store.OID]struct{}{}
		if _, err := repo.Walk(common, skip); err != nil {
			fail(w, err)
			return
		}
		if send, err = repo.Walk(req.Wants, skip); err != nil {
			fail(w, err)
			return
		}
	}

	w.Header().Set("Content-Type", "application/x-"+serviceUpload+"-result")
	w.Header().Set("Cache-Control", "no-cache")
	for _, c := range common {
		pkt.WriteString(w, "ACK "+c.String()+" common\n")
	}
	if !req.Done {
		pkt.WriteString(w, "NAK\n")
		return
	}
	if len(common) > 0 {
		pkt.WriteString(w, "ACK "+common[len(common)-1].String()+"\n")
	} else {
		pkt.WriteString(w, "NAK\n")
	}
	if err := pack.Write(w, send, repo.Read); err != nil {
		// The answer has already started, so the only honest signal left is
		// to cut the connection; the client then sees a broken pack.
		s.logf("fetch aborted repo=%s: %s", repo.Name(), pferr.Public(err, "the pack could not be sent"))
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	in, err := body(r, serviceReceive, MaxPush)
	if err != nil {
		fail(w, err)
		return
	}
	pr := pkt.NewReader(in)
	cmds, caps, err := ParseCommands(pr)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-"+serviceReceive+"-result")
	w.Header().Set("Cache-Control", "no-cache")
	if len(cmds) == 0 {
		return
	}

	repo.LockPush()
	defer repo.UnlockPush()

	needPack := false
	for _, c := range cmds {
		needPack = needPack || !c.New.IsZero()
	}
	var unpackErr error
	if needPack {
		unpackErr = unpack(repo, in)
	}
	if unpackErr != nil {
		// Let the client finish sending so that it reads the answer.
		io.Copy(io.Discard, in)
	}

	var report []string
	mirrorDue := false
	prefix := s.repoPrefix(repo)
	if unpackErr != nil {
		pe := pferr.Public(unpackErr, "the pack could not be stored")
		if prefix != "" {
			s.logf("push refused repo=%s: %s", repo.Name(), pe)
		} else {
			s.logf("push refused: %s", pe)
		}
		report = append(report, "unpack "+pe.Error())
		for _, c := range cmds {
			report = append(report, "ng "+c.Ref+" "+pe.Error())
		}
	} else {
		report = append(report, "unpack ok")
		done := map[string]bool{}
		for _, c := range cmds {
			var err error
			if done[c.Ref] {
				err = pferr.Parsef("the push names the same ref twice")
			} else {
				err = s.update(r.Context(), repo, c)
			}
			done[c.Ref] = true
			if err != nil {
				pe := pferr.Public(err, "the ref could not be updated")
				s.logf("update refused %sref=%q: %s", prefix, c.Ref, pe)
				report = append(report, "ng "+c.Ref+" "+pe.Error())
				continue
			}
			if mirror.IsMirrorRef(c.Ref) {
				mirrorDue = true
			}
			report = append(report, "ok "+c.Ref)
		}
	}
	if mirrorDue && s.Mirror != nil {
		s.Mirror.Enqueue(repo)
	}
	if !hasCap(caps, "report-status") {
		return
	}
	for _, line := range report {
		pkt.WriteString(w, line+"\n")
	}
	pkt.Flush(w)
}

func (s *Server) repoPrefix(repo *store.Repo) string {
	if s.Catalog != nil {
		return "repo=" + repo.Name() + " "
	}
	return ""
}

// unpack reads the pack of a push into a quarantine, checks it and moves
// it into the repository.
func unpack(repo *store.Repo, in io.Reader) error {
	q, err := repo.NewQuarantine()
	if err != nil {
		return err
	}
	if _, err := pack.Read(in, q); err != nil {
		q.Discard()
		return err
	}
	return q.Commit()
}

// update applies one ref update and announces it when the rule says so.
func (s *Server) update(ctx context.Context, repo *store.Repo, c Command) error {
	// Read the current value only for names that pass the name rule; Decide
	// reports a bad name itself.
	var current store.OID
	if store.CheckRefName(c.Ref) == nil {
		var err error
		if current, _, err = repo.Ref(c.Ref); err != nil {
			return err
		}
	}
	var kind store.Type
	if !c.New.IsZero() {
		var err error
		if kind, _, err = repo.TypeOf(c.New); err != nil {
			return err
		}
	}
	err := Decide(Update{Cmd: c, Current: current, NewKind: kind, FastForward: func() (bool, error) {
		return repo.IsAncestor(c.Old, c.New)
	}})
	if err != nil {
		return err
	}
	if err := repo.SetRef(c.Ref, c.New); err != nil {
		return err
	}
	prefix := s.repoPrefix(repo)
	s.logf("update accepted %sref=%s before=%s after=%s", prefix, c.Ref, c.Old, c.New)

	var message []byte
	if c.Ref == notify.MainBranch {
		_, data, err := repo.Read(c.New)
		if err != nil {
			return nil // the update itself has landed
		}
		if info, err := store.ParseCommit(data); err == nil {
			message = info.Message
		}
	}
	event, announce := notify.Decide(repo.Name(), c.Ref, c.Old, c.New, message)
	switch {
	case !announce:
		s.logf("notification not due %sref=%s", prefix, c.Ref)
	case s.Notify == nil:
		s.logf("notification off %sref=%s", prefix, c.Ref)
	default:
		nctx, cancel := context.WithTimeout(ctx, notify.Timeout)
		defer cancel()
		if err := s.Notify.Send(nctx, event); err != nil {
			s.logf("notification failed %sref=%s: %s", prefix, c.Ref, pferr.Public(err, "the push notification failed"))
		} else {
			s.logf("notification sent %sref=%s", prefix, c.Ref)
		}
	}
	return nil
}
