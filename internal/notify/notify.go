// Package notify decides when a push is announced and sends the
// announcement.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/clearedstack/plainforge/internal/pferr"
	"github.com/clearedstack/plainforge/internal/store"
)

// MainBranch is the one branch whose changes are announced.
const MainBranch = "refs/heads/main"

// SkipMarker in the message of the new tip commit of main stops the
// announcement, so that a commit written back by the build does not start
// another build.
const SkipMarker = "[skip-ci]"

// Timeout is how long one announcement may take.
const Timeout = 5 * time.Second

// Event is one announced change. The field order is the order in the JSON
// body.
type Event struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
}

// Decide applies the announcement rule to one accepted ref update.
//
//   - A change of refs/heads/main is announced, unless the message of the
//     new tip commit contains "[skip-ci]".
//   - A new tag is announced.
//   - Nothing else is announced.
//
// tipMessage is the message of the commit the branch now points at; it is
// only looked at for main.
func Decide(repository, ref string, before, after store.OID, tipMessage []byte) (Event, bool) {
	e := Event{Repository: repository, Ref: ref, Before: before.String(), After: after.String()}
	switch {
	case ref == MainBranch:
		return e, !bytes.Contains(tipMessage, []byte(SkipMarker))
	case strings.HasPrefix(ref, "refs/tags/"):
		return e, before.IsZero()
	}
	return e, false
}

// Body is the exact request body sent for an event.
func Body(e Event) []byte {
	b, _ := json.Marshal(e)
	return append(b, '\n')
}

// Sender delivers events.
type Sender interface {
	Send(ctx context.Context, e Event) error
}

// HTTP posts each event as JSON to one fixed URL. It tries once.
type HTTP struct {
	URL    string
	client *http.Client
}

// NewHTTP returns a sender for the given URL.
func NewHTTP(url string) *HTTP {
	return &HTTP{URL: url, client: &http.Client{
		Timeout: Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Send posts one event. The errors it returns never contain the URL.
func (h *HTTP) Send(ctx context.Context, e Event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(Body(e)))
	if err != nil {
		return pferr.Parsef("the notification address is not a usable URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plainforge")
	resp, err := h.client.Do(req)
	if err != nil {
		return pferr.Timeoutf("the push notification was not delivered in time")
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return pferr.Needsf("the push notification was answered with status %d", resp.StatusCode)
	}
	return nil
}
