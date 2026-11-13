package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/clearedstack/plainforge/internal/notify"
	"github.com/clearedstack/plainforge/internal/pack"
	"github.com/clearedstack/plainforge/internal/store"
)

var update = flag.Bool("update", false, "rewrite the expected answers marked FILL in testdata/exchange.json")

// recorder keeps every announcement instead of sending it.
type recorder struct {
	mu     sync.Mutex
	bodies []string
	err    error
}

func (r *recorder) Send(_ context.Context, e notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, string(notify.Body(e)))
	return r.err
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.bodies
	r.bodies = nil
	if out == nil {
		out = []string{}
	}
	return out
}

// listSink records the objects of a pack in the order they arrive.
type listSink struct {
	list []string
	data map[store.OID][]byte
	kind map[store.OID]store.Type
}

func (l *listSink) Write(t store.Type, data []byte) (store.OID, error) {
	o := store.Hash(t, data)
	l.list = append(l.list, o.String()+" "+t.String())
	l.data[o], l.kind[o] = data, t
	return o, nil
}

func (l *listSink) Read(o store.OID) (store.Type, []byte, bool, error) {
	_, ok := l.data[o]
	return l.kind[o], l.data[o], ok, nil
}

func newTestServer(t *testing.T) (*Server, *httptest.Server, *recorder, *bytes.Buffer) {
	t.Helper()
	repo, err := store.Open(filepath.Join(t.TempDir(), "example.git"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	var logs bytes.Buffer
	srv := &Server{Repo: repo, Notify: rec, Log: log.New(&logs, "", 0)}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	return srv, hs, rec, &logs
}

type step struct {
	Name            string    `json:"name"`
	Method          string    `json:"method"`
	Path            string    `json:"path"`
	ContentType     string    `json:"content_type,omitempty"`
	ContentEncoding string    `json:"content_encoding,omitempty"`
	Lines           []string  `json:"lines,omitempty"`
	Pack            string    `json:"pack,omitempty"`
	PackHex         string    `json:"pack_hex,omitempty"`
	Status          int       `json:"status"`
	Type            string    `json:"type"`
	Body            string    `json:"body"`
	Objects         []string  `json:"objects,omitempty"`
	Notifications   *[]string `json:"notifications,omitempty"`
}

func (s step) request(t *testing.T) []byte {
	body := pktLines(s.Lines)
	if s.Pack != "" {
		b, err := os.ReadFile(filepath.Join("..", "pack", "testdata", s.Pack))
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, b...)
	}
	extra, err := hex.DecodeString(s.PackHex)
	if err != nil {
		t.Fatal(err)
	}
	return append(body, extra...)
}

// TestSavedExchange replays a saved conversation against one repository,
// in order, and compares every answer byte for byte. Where an answer ends
// in a pack, the text before the pack is compared exactly and the pack is
// decoded and compared object by object, in order.
func TestSavedExchange(t *testing.T) {
	var steps []step
	loadJSON(t, "exchange.json", &steps)
	_, hs, rec, _ := newTestServer(t)

	for i := range steps {
		s := &steps[i]
		t.Run(s.Name, func(t *testing.T) {
			// Raw path, so that ".." reaches the server unchanged.
			req, err := http.NewRequest(s.Method, hs.URL, bytes.NewReader(s.request(t)))
			if err != nil {
				t.Fatal(err)
			}
			path, query, _ := strings.Cut(s.Path, "?")
			req.URL.Opaque = path
			req.URL.RawQuery = query
			if s.ContentType != "" {
				req.Header.Set("Content-Type", s.ContentType)
			}
			if s.ContentEncoding != "" {
				req.Header.Set("Content-Encoding", s.ContentEncoding)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != s.Status || resp.Header.Get("Content-Type") != s.Type {
				t.Errorf("status %d, type %q; want %d, %q", resp.StatusCode, resp.Header.Get("Content-Type"), s.Status, s.Type)
			}
			text, objects := string(got), []string(nil)
			if s.Objects != nil {
				at := bytes.Index(got, []byte("PACK"))
				if at < 0 {
					t.Fatalf("no pack in the answer %q", got)
				}
				sink := &listSink{data: map[store.OID][]byte{}, kind: map[store.OID]store.Type{}}
				if _, err := pack.Read(bytes.NewReader(got[at:]), sink); err != nil {
					t.Fatalf("the pack in the answer is not readable: %v", err)
				}
				text, objects = string(got[:at]), sink.list
			}
			if *update && s.Body == "FILL" {
				s.Body = text
			}
			if *update && len(s.Objects) == 1 && s.Objects[0] == "FILL" {
				s.Objects = objects
			}
			if text != s.Body {
				t.Errorf("answer %q\nwant   %q", text, s.Body)
			}
			if !reflect.DeepEqual(objects, s.Objects) {
				t.Errorf("objects %q\nwant    %q", objects, s.Objects)
			}
			sent := rec.take()
			if s.Notifications != nil && !reflect.DeepEqual(sent, *s.Notifications) {
				t.Errorf("notifications %q\nwant          %q", sent, *s.Notifications)
			}
			if s.Notifications == nil && len(sent) != 0 {
				t.Errorf("unexpected notifications %q", sent)
			}
		})
	}
	if *update {
		b, _ := json.MarshalIndent(steps, "", "  ")
		if err := os.WriteFile("testdata/exchange.json", append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func post(t *testing.T, url, contentType string, header map[string]string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func names(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../pack/testdata/names.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		k, v, _ := strings.Cut(line, " ")
		m[k] = v
	}
	return m
}

const zeroName = "0000000000000000000000000000000000000000"

func pushFixture(t *testing.T, hs *httptest.Server, old, new, ref, packFile string) string {
	t.Helper()
	body := pktLines([]string{old + " " + new + " " + ref + "\x00report-status", "FLUSH"})
	p, err := os.ReadFile("../pack/testdata/" + packFile)
	if err != nil {
		t.Fatal(err)
	}
	_, answer := post(t, hs.URL+"/example.git/git-receive-pack", "application/x-git-receive-pack-request", nil, append(body, p...))
	return answer
}

// Git compresses large fetch requests with gzip; the answer must be the
// same as for the plain request.
func TestGzipRequest(t *testing.T) {
	_, hs, _, _ := newTestServer(t)
	n := names(t)
	pushFixture(t, hs, zeroName, n["B"], "refs/heads/main", "first-ofs.pack")
	plain := pktLines([]string{"want " + n["B"] + " multi_ack_detailed\n", "FLUSH", "have " + n["A"] + "\n", "FLUSH"})
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	zw.Write(plain)
	zw.Close()
	url, ct := hs.URL+"/example.git/git-upload-pack", "application/x-git-upload-pack-request"
	s1, a1 := post(t, url, ct, nil, plain)
	s2, a2 := post(t, url, ct, map[string]string{"Content-Encoding": "gzip"}, zipped.Bytes())
	want := "0038ACK " + n["A"] + " common\n0008NAK\n"
	if s1 != 200 || s2 != 200 || a1 != want || a2 != want {
		t.Fatalf("plain: %d %q\ngzip:  %d %q", s1, a1, s2, a2)
	}
	s3, a3 := post(t, url, ct, map[string]string{"Content-Encoding": "gzip"}, plain)
	if s3 != 400 || a3 != "E_PARSE: the request body is not valid gzip data\n" {
		t.Fatalf("not gzip: %d %q", s3, a3)
	}
}

// A push still lands when the announcement fails, and the log line uses a
// public code.
func TestPushLandsWhenNotificationFails(t *testing.T) {
	srv, hs, rec, logs := newTestServer(t)
	rec.err = notify.NewHTTP("http://127.0.0.1:1/").Send(context.Background(), notify.Event{})
	if rec.err == nil {
		t.Skip("something answers on port 1 of this machine")
	}
	n := names(t)
	answer := pushFixture(t, hs, zeroName, n["B"], "refs/heads/main", "first-ofs.pack")
	if answer != "000eunpack ok\n0017ok refs/heads/main\n0000" {
		t.Fatalf("answer %q", answer)
	}
	got, ok, err := srv.Repo.Ref("refs/heads/main")
	if err != nil || !ok || got.String() != n["B"] {
		t.Fatalf("main is %v %v %v", got, ok, err)
	}
	want := "update accepted ref=refs/heads/main before=" + zeroName + " after=" + n["B"] + "\n" +
		"notification failed ref=refs/heads/main: E_TIMEOUT: the push notification was not delivered in time\n"
	if logs.String() != want {
		t.Fatalf("log:\n%swant:\n%s", logs.String(), want)
	}
}

// With no notifier configured a push lands and the log says so.
func TestPushWithoutNotifier(t *testing.T) {
	srv, hs, _, logs := newTestServer(t)
	srv.Notify = nil
	n := names(t)
	pushFixture(t, hs, zeroName, n["B"], "refs/heads/main", "first-ofs.pack")
	if !strings.HasSuffix(logs.String(), "notification off ref=refs/heads/main\n") {
		t.Fatalf("log: %s", logs.String())
	}
}

// A refused pack leaves no object and no temporary file behind.
func TestRefusedPushLeavesNothing(t *testing.T) {
	srv, hs, rec, _ := newTestServer(t)
	n := names(t)
	answer := pushFixture(t, hs, zeroName, n["C"], "refs/heads/main", "second-thin.pack")
	want := "0047unpack E_PARSE: the pack holds a delta whose base is not available\n" +
		"0053ng refs/heads/main E_PARSE: the pack holds a delta whose base is not available\n0000"
	if answer != want {
		t.Fatalf("answer %q\nwant   %q", answer, want)
	}
	refs, _ := srv.Repo.Refs()
	dir := srv.Repo.Dir()
	fans, _ := filepath.Glob(filepath.Join(dir, "objects", "??"))
	tmp, _ := os.ReadDir(filepath.Join(dir, "plainforge-tmp"))
	if len(refs) != 0 || len(fans) != 0 || len(tmp) != 0 || len(rec.take()) != 0 {
		t.Fatalf("left behind: refs %v, objects %v, temporary %v", refs, fans, tmp)
	}
}

// A request larger than the limit is refused with a public error.
func TestRequestTooLarge(t *testing.T) {
	_, hs, _, _ := newTestServer(t)
	n := names(t)
	pushFixture(t, hs, zeroName, n["B"], "refs/heads/main", "first-ofs.pack")
	lines := []string{"want " + n["B"] + " multi_ack_detailed\n", "FLUSH"}
	for i := 0; i <= MaxFetchRequest/50; i++ {
		lines = append(lines, "have "+strings.Repeat("e", 40)+"\n")
	}
	status, answer := post(t, hs.URL+"/example.git/git-upload-pack", "application/x-git-upload-pack-request", nil, pktLines(lines))
	if status != 422 || answer != "E_NEEDS_SUBTOOL: the request is larger than Plainforge accepts\n" {
		t.Fatalf("status %d %q", status, answer)
	}
}
