package mirror

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/pkt"
	"github.com/ClearedStack/plainforge/internal/store"
)

func TestRuleFixtures(t *testing.T) {
	var cases []struct {
		Ref    string `json:"ref"`
		Mirror bool   `json:"mirror"`
	}
	b, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := IsMirrorRef(c.Ref); got != c.Mirror {
			t.Errorf("IsMirrorRef(%q) = %v, want %v", c.Ref, got, c.Mirror)
		}
	}
}

func TestParseAdvertisementAndReportStatus(t *testing.T) {
	var adv bytes.Buffer
	pkt.WriteString(&adv, "# service=git-receive-pack\n")
	pkt.Flush(&adv)
	pkt.WriteString(&adv, strings.Repeat("0", 40)+" capabilities^{}\x00report-status ofs-delta agent=example\n")
	pkt.Flush(&adv)

	refs, caps, err := ParseAdvertisement(&adv)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 || len(caps) != 3 {
		t.Fatalf("empty advertisement: refs=%v caps=%v", refs, caps)
	}

	// Sideband status report with progress on channel 2 and status on channel 1.
	var inner bytes.Buffer
	pkt.WriteString(&inner, "unpack ok\n")
	pkt.WriteString(&inner, "ok refs/heads/main\n")
	pkt.Flush(&inner)

	var outer bytes.Buffer
	pkt.Write(&outer, append([]byte{2}, []byte("remote: counting\n")...))
	pkt.Write(&outer, append([]byte{1}, inner.Bytes()...))
	pkt.Flush(&outer)

	if err := ParseReportStatus(&outer, []string{"refs/heads/main"}); err != nil {
		t.Fatalf("sideband ok: %v", err)
	}

	var refused bytes.Buffer
	pkt.WriteString(&refused, "unpack ok\n")
	pkt.WriteString(&refused, "ng refs/heads/main non-fast-forward\n")
	pkt.Flush(&refused)
	err = ParseReportStatus(&refused, []string{"refs/heads/main"})
	if err == nil || err.Error() != "E_NEEDS_SUBTOOL: the mirror refused a ref update" {
		t.Fatalf("refused status: %v", err)
	}
}

func seedCommit(t *testing.T, r *store.Repo, content string, parents ...store.OID) store.OID {
	t.Helper()
	q, err := r.NewQuarantine()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := q.Write(store.Blob, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := q.Write(store.Tree, append([]byte("100644 README.txt\x00"), blob[:]...))
	if err != nil {
		t.Fatal(err)
	}
	var hdr strings.Builder
	hdr.WriteString("tree " + tree.String() + "\n")
	for _, p := range parents {
		hdr.WriteString("parent " + p.String() + "\n")
	}
	hdr.WriteString("author Example Author <author@example.invalid> 978307200 +0000\n")
	hdr.WriteString("committer Example Author <author@example.invalid> 978307200 +0000\n\n")
	hdr.WriteString("Made-up change\n")
	commit, err := q.Write(store.Commit, []byte(hdr.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Commit(); err != nil {
		t.Fatal(err)
	}
	return commit
}

func TestCredentialGuardAndStatusCodes(t *testing.T) {
	repo, err := store.Open(filepath.Join(t.TempDir(), "example.git"))
	if err != nil {
		t.Fatal(err)
	}
	c1 := seedCommit(t, repo, "hello\n")
	if err := repo.SetRef("refs/heads/main", c1); err != nil {
		t.Fatal(err)
	}

	tokPath := filepath.Join(t.TempDir(), "mirror-token")
	if err := os.WriteFile(tokPath, []byte("made-up-secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Refuses to send credentials over plaintext HTTP unless explicitly allowed in tests.
	insec := NewHTTP("http://127.0.0.1:9999")
	insec.TokenFile = tokPath
	err = insec.Sync(context.Background(), repo)
	if err == nil || err.Error() != "E_PARSE: the mirror credential requires an https address" {
		t.Fatalf("cleartext guard: %v", err)
	}

	cases := []struct {
		name   string
		status int
		code   pferr.Code
		want   string
	}{
		{"rate limited 429", http.StatusTooManyRequests, pferr.Timeout, "E_TIMEOUT: the mirror answered with status 429"},
		{"unavailable 503", http.StatusServiceUnavailable, pferr.Timeout, "E_TIMEOUT: the mirror answered with status 503"},
		{"unauthorized 401", http.StatusUnauthorized, pferr.Parse, "E_PARSE: the mirror refused the credential"},
		{"missing repo 404", http.StatusNotFound, pferr.NeedsSubtool, "E_NEEDS_SUBTOOL: the mirror has no repository for this name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			m := NewHTTP(srv.URL)
			m.TokenFile = tokPath
			m.AllowInsecureHTTP = true
			m.Timeout = 2 * time.Second
			err := m.Sync(context.Background(), repo)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if pferr.Public(err, "").Code != tc.code {
				t.Fatalf("code = %s, want %s", pferr.Public(err, "").Code, tc.code)
			}
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:made-up-secret-token"))
			if gotAuth != wantAuth {
				t.Fatalf("auth = %q, want %q", gotAuth, wantAuth)
			}
			if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "made-up-secret-token") || strings.Contains(err.Error(), tokPath) {
				t.Fatalf("error leaked sensitive detail: %v", err)
			}
		})
	}
}

type flakyPusher struct {
	fail atomic.Bool
	runs atomic.Int32
}

func (f *flakyPusher) Sync(_ context.Context, _ *store.Repo) error {
	f.runs.Add(1)
	if f.fail.Load() {
		return pferr.Timeoutf("the mirror answered with status 429")
	}
	return nil
}

func TestQueuePersistsAndRecoversAcrossRestart(t *testing.T) {
	repo, err := store.Open(filepath.Join(t.TempDir(), "trawl.git"))
	if err != nil {
		t.Fatal(err)
	}
	fp := &flakyPusher{}
	fp.fail.Store(true)

	q1 := NewQueue(fp, nil)
	q1.Enqueue(repo)
	if rem := q1.SyncOnce(context.Background()); rem != 1 {
		t.Fatalf("remaining while rate-limited = %d, want 1", rem)
	}

	// Simulate pod restart: a fresh Queue recovers the on-disk pending marker.
	fp.fail.Store(false)
	q2 := NewQueue(fp, nil)
	q2.Recover([]*store.Repo{repo})
	if q2.PendingCount() != 1 {
		t.Fatalf("recovered pending = %d, want 1", q2.PendingCount())
	}
	if rem := q2.SyncOnce(context.Background()); rem != 0 {
		t.Fatalf("remaining after recovery = %d, want 0", rem)
	}
	if _, err := os.Stat(markerPath(repo)); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after sync: %v", err)
	}
}
