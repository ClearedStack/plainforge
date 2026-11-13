package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clearedstack/plainforge/internal/store"
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
