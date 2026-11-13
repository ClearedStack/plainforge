package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stall sends the start of a request and then nothing more, and returns
// what the server answers.
func stall(t *testing.T, addr, path, contentType, start string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "POST "+path+" HTTP/1.1\r\nHost: example\r\nContent-Type: "+contentType+
		"\r\nContent-Length: 100000\r\n\r\n"+start)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A client that stops sending is answered with E_TIMEOUT, and a push that
// stalls changes nothing.
func TestStalledRequests(t *testing.T) {
	srv, hs, rec, _ := newTestServer(t)
	srv.Timeout = 200 * time.Millisecond
	addr := strings.TrimPrefix(hs.URL, "http://")
	n := names(t)

	status, answer := stall(t, addr, "/example.git/git-upload-pack", "application/x-git-upload-pack-request", "0032want ")
	if status != 504 || answer != "E_TIMEOUT: the request took too long\n" {
		t.Fatalf("stalled fetch: %d %q", status, answer)
	}

	start := string(pktLines([]string{zeroName + " " + n["B"] + " refs/heads/main\x00report-status", "FLUSH"})) + "PACK"
	status, answer = stall(t, addr, "/example.git/git-receive-pack", "application/x-git-receive-pack-request", start)
	want := "0030unpack E_TIMEOUT: the request took too long\n" +
		"003cng refs/heads/main E_TIMEOUT: the request took too long\n0000"
	if status != 200 || answer != want {
		t.Fatalf("stalled push: %d %q", status, answer)
	}
	if refs, _ := srv.Repo.Refs(); len(refs) != 0 || len(rec.take()) != 0 {
		t.Fatalf("a stalled push changed the repository: %v", refs)
	}
}
