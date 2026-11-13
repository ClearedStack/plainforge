// Package server serves one bare repository over Git's smart HTTP protocol.
package server

import (
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/clearedstack/plainforge/internal/notify"
	"github.com/clearedstack/plainforge/internal/pack"
	"github.com/clearedstack/plainforge/internal/pferr"
	"github.com/clearedstack/plainforge/internal/pkt"
	"github.com/clearedstack/plainforge/internal/store"
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

// Server serves one repository.
type Server struct {
	Repo *store.Repo
	// Notify announces accepted pushes. Nil turns announcements off.
	Notify notify.Sender
	// Log receives one line per accepted or refused ref update.
	Log *log.Logger
	// Timeout replaces RequestTimeout when it is not zero.
	Timeout time.Duration

	// push makes sure one push at a time changes the repository.
	push sync.Mutex
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(format, a...)
	}
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

	rest, ok := strings.CutPrefix(r.URL.Path, "/"+s.Repo.Name()+".git/")
	if !ok {
		failStatus(w, http.StatusNotFound, pferr.Needsf("there is no such repository or page"))
		return
	}
	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		s.infoRefs(w, r)
	case rest == serviceUpload && r.Method == http.MethodPost:
		s.uploadPack(w, r)
	case rest == serviceReceive && r.Method == http.MethodPost:
		s.receivePack(w, r)
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

func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != serviceUpload && service != serviceReceive {
		fail(w, pferr.Needsf("only the smart HTTP protocol is supported"))
		return
	}
	head, err := s.Repo.HeadTarget()
	if err != nil {
		fail(w, err)
		return
	}
	refs, err := s.Repo.Refs()
	if err != nil {
		fail(w, err)
		return
	}
	var peeled []Peeled
	if service == serviceUpload {
		if peeled, err = s.peel(refs); err != nil {
			fail(w, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(Advertise(service, head, refs, peeled))
}

// peel follows annotated tags to the object behind them.
func (s *Server) peel(refs []store.Ref) ([]Peeled, error) {
	var out []Peeled
	for _, r := range refs {
		oid := r.OID
		for depth := 0; ; depth++ {
			t, data, err := s.Repo.Read(oid)
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

// walk lists every object reachable from the roots, in a fixed order,
// leaving out the objects in skip. Every object it lists is added to skip.
func (s *Server) walk(roots []store.OID, skip map[store.OID]struct{}) ([]store.OID, error) {
	var out []store.OID
	stack := make([]store.OID, 0, 64)
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, roots[i])
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, seen := skip[oid]; seen {
			continue
		}
		skip[oid] = struct{}{}
		out = append(out, oid)
		t, data, err := s.Repo.Read(oid)
		if err != nil {
			return nil, err
		}
		links, err := store.Links(t, data)
		if err != nil {
			return nil, err
		}
		for i := len(links) - 1; i >= 0; i-- {
			stack = append(stack, links[i].OID)
		}
	}
	return out, nil
}

func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request) {
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
	refs, err := s.Repo.Refs()
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
		if !seen[have] && s.Repo.Has(have) {
			common = append(common, have)
		}
		seen[have] = true
	}

	var send []store.OID
	if req.Done {
		skip := map[store.OID]struct{}{}
		if _, err := s.walk(common, skip); err != nil {
			fail(w, err)
			return
		}
		if send, err = s.walk(req.Wants, skip); err != nil {
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
	if err := pack.Write(w, send, s.Repo.Read); err != nil {
		// The answer has already started, so the only honest signal left is
		// to cut the connection; the client then sees a broken pack.
		s.logf("fetch aborted: %s", pferr.Public(err, "the pack could not be sent"))
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) receivePack(w http.ResponseWriter, r *http.Request) {
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

	s.push.Lock()
	defer s.push.Unlock()

	needPack := false
	for _, c := range cmds {
		needPack = needPack || !c.New.IsZero()
	}
	var unpackErr error
	if needPack {
		unpackErr = s.unpack(in)
	}
	if unpackErr != nil {
		// Let the client finish sending so that it reads the answer.
		io.Copy(io.Discard, in)
	}

	var report []string
	if unpackErr != nil {
		pe := pferr.Public(unpackErr, "the pack could not be stored")
		s.logf("push refused: %s", pe)
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
				err = s.update(r.Context(), c)
			}
			done[c.Ref] = true
			if err != nil {
				pe := pferr.Public(err, "the ref could not be updated")
				s.logf("update refused ref=%q: %s", c.Ref, pe)
				report = append(report, "ng "+c.Ref+" "+pe.Error())
				continue
			}
			report = append(report, "ok "+c.Ref)
		}
	}
	if !hasCap(caps, "report-status") {
		return
	}
	for _, line := range report {
		pkt.WriteString(w, line+"\n")
	}
	pkt.Flush(w)
}

// unpack reads the pack of a push into a quarantine, checks it and moves
// it into the repository.
func (s *Server) unpack(in io.Reader) error {
	q, err := s.Repo.NewQuarantine()
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
func (s *Server) update(ctx context.Context, c Command) error {
	// Read the current value only for names that pass the name rule; Decide
	// reports a bad name itself.
	var current store.OID
	if store.CheckRefName(c.Ref) == nil {
		var err error
		if current, _, err = s.Repo.Ref(c.Ref); err != nil {
			return err
		}
	}
	var kind store.Type
	if !c.New.IsZero() {
		var err error
		if kind, _, err = s.Repo.TypeOf(c.New); err != nil {
			return err
		}
	}
	err := Decide(Update{Cmd: c, Current: current, NewKind: kind, FastForward: func() (bool, error) {
		return s.isAncestor(c.Old, c.New)
	}})
	if err != nil {
		return err
	}
	if err := s.Repo.SetRef(c.Ref, c.New); err != nil {
		return err
	}
	s.logf("update accepted ref=%s before=%s after=%s", c.Ref, c.Old, c.New)

	var message []byte
	if c.Ref == notify.MainBranch {
		_, data, err := s.Repo.Read(c.New)
		if err != nil {
			return nil // the update itself has landed
		}
		if info, err := store.ParseCommit(data); err == nil {
			message = info.Message
		}
	}
	event, announce := notify.Decide(s.Repo.Name(), c.Ref, c.Old, c.New, message)
	switch {
	case !announce:
		s.logf("notification not due ref=%s", c.Ref)
	case s.Notify == nil:
		s.logf("notification off ref=%s", c.Ref)
	default:
		nctx, cancel := context.WithTimeout(ctx, notify.Timeout)
		defer cancel()
		if err := s.Notify.Send(nctx, event); err != nil {
			s.logf("notification failed ref=%s: %s", c.Ref, pferr.Public(err, "the push notification failed"))
		} else {
			s.logf("notification sent ref=%s", c.Ref)
		}
	}
	return nil
}

// isAncestor reports whether old can be reached from tip by following
// parents.
func (s *Server) isAncestor(old, tip store.OID) (bool, error) {
	seen := map[store.OID]bool{tip: true}
	queue := []store.OID{tip}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == old {
			return true, nil
		}
		t, data, err := s.Repo.Read(oid)
		if err != nil {
			return false, err
		}
		if t != store.Commit {
			return false, pferr.Parsef("a commit has a parent that is not a commit")
		}
		info, err := store.ParseCommit(data)
		if err != nil {
			return false, err
		}
		for _, p := range info.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false, nil
}
