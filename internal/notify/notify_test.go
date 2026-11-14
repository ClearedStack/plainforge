package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

func TestRuleFixtures(t *testing.T) {
	var cases []struct {
		Name, Ref, Before, After, Body string
		TipMessage                     string `json:"tip_message"`
	}
	b, err := os.ReadFile("testdata/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			before, err1 := store.ParseOID(c.Before)
			after, err2 := store.ParseOID(c.After)
			if err1 != nil || err2 != nil {
				t.Fatal(err1, err2)
			}
			got := ""
			if e, ok := Decide("example", c.Ref, before, after, []byte(c.TipMessage)); ok {
				got = string(Body(e))
			}
			if got != c.Body {
				t.Fatalf("got %q, want %q", got, c.Body)
			}
		})
	}
}

var sample = Event{Repository: "example", Ref: "refs/heads/main",
	Before: strings.Repeat("0", 40), After: strings.Repeat("1", 40)}

func TestSendPostsExactBody(t *testing.T) {
	var gotBody, gotType, gotMethod string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod = string(b), r.Header.Get("Content-Type"), r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()
	if err := NewHTTP(peer.URL).Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	want := `{"repository":"example","ref":"refs/heads/main","before":"0000000000000000000000000000000000000000","after":"1111111111111111111111111111111111111111"}` + "\n"
	if gotBody != want || gotType != "application/json" || gotMethod != "POST" {
		t.Fatalf("got %s %q %q", gotMethod, gotType, gotBody)
	}
}

// Failures use the public codes and never repeat the address.
func TestSendFailures(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer refuse.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, refuse.URL, http.StatusFound)
	}))
	defer redirect.Close()
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	cases := []struct {
		name, url, want string
	}{
		{"status 500", refuse.URL, "E_NEEDS_SUBTOOL: the push notification was answered with status 500"},
		{"redirect is not followed", redirect.URL, "E_NEEDS_SUBTOOL: the push notification was answered with status 302"},
		{"too slow", slow.URL, "E_TIMEOUT: the push notification was not delivered in time"},
		{"nobody listening", goneURL, "E_TIMEOUT: the push notification was not delivered in time"},
		{"not a URL", "://nowhere", "E_PARSE: the notification address is not a usable URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := NewHTTP(c.url)
			h.client.Timeout = 200 * time.Millisecond
			err := h.Send(context.Background(), sample)
			if err == nil || err.Error() != c.want {
				t.Fatalf("got %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("the error names an address: %v", err)
			}
		})
	}
}

func TestKeelworksContractFixtures(t *testing.T) {
	var cases []struct {
		Name      string `json:"name"`
		Token     string `json:"token"`
		Event     Event  `json:"event"`
		Status    int    `json:"status"`
		Response  string `json:"response"`
		WantAuth  string `json:"want_auth"`
		WantBody  string `json:"want_body"`
		WantError string `json:"want_error"`
		Retriable bool   `json:"retriable"`
	}
	b, err := os.ReadFile("testdata/keelworks_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var gotPath, gotBody string
			var gotAuth []string
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotPath = r.URL.Path
				gotBody = string(raw)
				gotAuth = r.Header.Values("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.Status)
				io.WriteString(w, c.Response)
			}))
			defer peer.Close()

			tokFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokFile, []byte(c.Token+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			h := NewHTTP(peer.URL + "/webhook")
			h.TokenFile = tokFile
			err := h.Send(context.Background(), c.Event)

			if gotPath != "/webhook" {
				t.Fatalf("path %q, want /webhook", gotPath)
			}
			if len(gotAuth) != 1 || gotAuth[0] != c.WantAuth {
				t.Fatalf("Authorization %q, want [%q]", gotAuth, c.WantAuth)
			}
			if gotBody != c.WantBody {
				t.Fatalf("body %q, want %q", gotBody, c.WantBody)
			}
			gotErr := ""
			retriable := false
			if err != nil {
				gotErr = err.Error()
				retriable = pferr.Public(err, "").Code == pferr.Timeout
			}
			if gotErr != c.WantError {
				t.Fatalf("error %q, want %q", gotErr, c.WantError)
			}
			if retriable != c.Retriable {
				t.Fatalf("retriable %v, want %v", retriable, c.Retriable)
			}
		})
	}
}

func TestTokenRotationAndSanitization(t *testing.T) {
	var seen []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer peer.Close()

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "projected-token")
	os.WriteFile(tokPath, []byte("first-token.v1\n"), 0o600)

	h := NewHTTP(peer.URL + "/webhook")
	h.TokenFile = tokPath
	if err := h.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(tokPath, []byte("rotated-token.v2\n"), 0o600)
	if err := h.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer first-token.v1" || seen[1] != "Bearer rotated-token.v2" {
		t.Fatalf("seen headers %q", seen)
	}

	// Malformed or missing token files never leak the path or secret value.
	os.WriteFile(tokPath, []byte("bad token with spaces"), 0o600)
	err := h.Send(context.Background(), sample)
	if err == nil || err.Error() != "E_PARSE: the notification token is malformed" {
		t.Fatalf("bad token: %v", err)
	}
	if strings.Contains(err.Error(), tokPath) || strings.Contains(err.Error(), "bad token") {
		t.Fatalf("error leaked secret or path: %v", err)
	}

	h.TokenFile = filepath.Join(dir, "missing")
	err = h.Send(context.Background(), sample)
	if err == nil || err.Error() != "E_PARSE: the notification token could not be read" {
		t.Fatalf("missing token: %v", err)
	}
}

func TestSpoolRetriesTransientFailuresAndRecovers(t *testing.T) {
	var healthy atomic.Bool
	var delivered []string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"status":"refused","detail":"too many runs in progress"}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		delivered = append(delivered, string(b))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer peer.Close()

	spoolDir := filepath.Join(t.TempDir(), "spool")
	sp, err := OpenSpool(spoolDir, NewHTTP(peer.URL+"/webhook"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// While Keelworks returns 503, Send reports E_TIMEOUT and keeps the event on disk.
	err = sp.Send(context.Background(), sample)
	if err == nil || pferr.Public(err, "").Code != pferr.Timeout {
		t.Fatalf("expected E_TIMEOUT on 503, got %v", err)
	}
	if sp.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", sp.Pending())
	}

	// Reopening the spool after a restart still sees the queued notification
	// and delivers it once Keelworks becomes healthy.
	sp2, err := OpenSpool(spoolDir, NewHTTP(peer.URL+"/webhook"), nil)
	if err != nil {
		t.Fatal(err)
	}
	healthy.Store(true)
	if rem := sp2.Flush(context.Background()); rem != 0 {
		t.Fatalf("remaining after flush = %d, want 0", rem)
	}
	if len(delivered) != 1 || delivered[0] != string(Body(sample)) {
		t.Fatalf("delivered = %q", delivered)
	}
	if sp2.Pending() != 0 {
		t.Fatalf("pending = %d, want 0", sp2.Pending())
	}
}

