package forge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/plainforge/internal/pferr"
	"github.com/ClearedStack/plainforge/internal/store"
)

const (
	maxJSONRequest = 256 << 10
	maxForgeToken  = 4096
)

// RepoLookup resolves a repository name for an incoming Forge v1 request.
type RepoLookup func(name string) (*store.Repo, bool)

// MirrorTrigger queues outbound source mirroring for a repository when enabled.
type MirrorTrigger func(repo *store.Repo)

// ErrorBody is the JSON payload returned on every non-2xx Forge v1 response.
type ErrorBody struct {
	Code    pferr.Code `json:"code"`
	Message string     `json:"message"`
}

// Handler serves the Forge v1 HTTP contract under /v1/repos/.
type Handler struct {
	Store      *Store
	LookupRepo RepoLookup
	OnMirror   MirrorTrigger
	TokenFile  string
}

func (h *Handler) checkAuth(r *http.Request) error {
	if h.TokenFile == "" {
		return nil
	}
	f, err := os.Open(h.TokenFile)
	if err != nil {
		return pferr.Timeoutf("the forge credential could not be read")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxForgeToken+16))
	if err != nil {
		return pferr.Timeoutf("the forge credential could not be read")
	}
	expected := strings.TrimRight(string(b), "\r\n")
	if len(expected) == 0 || len(expected) > maxForgeToken || strings.ContainsAny(expected, " \t\r\n\x00") {
		return pferr.Parsef("the configured forge credential is malformed")
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return pferr.Parsef("missing or invalid bearer token")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, status int, err error) {
	pe := pferr.Public(err, "the forge request failed")
	if status == 0 {
		switch pe.Code {
		case pferr.Timeout:
			status = http.StatusServiceUnavailable
		case pferr.Parse:
			status = http.StatusBadRequest
		default:
			status = http.StatusUnprocessableEntity
		}
	}
	writeJSON(w, status, ErrorBody{Code: pe.Code, Message: pe.Msg})
}

func decodeJSON(r *http.Request, limit int64, dst any) error {
	ct := r.Header.Get("Content-Type")
	media, _, _ := strings.Cut(ct, ";")
	if strings.TrimSpace(media) != "application/json" {
		return pferr.Parsef("the request content type was not application/json")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return pferr.Timeoutf("the request body did not arrive in time")
	}
	if int64(len(data)) > limit {
		return pferr.Parsef("the request body is larger than allowed")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return pferr.Parsef("the request body is empty")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return pferr.Parsef("the request JSON is malformed")
	}
	if _, err := dec.Token(); err != io.EOF {
		return pferr.Parsef("the request JSON has trailing data")
	}
	return nil
}

// ServeHTTP routes /v1/repos/{repo}/... requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawPath != "" && strings.Contains(strings.ToLower(r.URL.RawPath), "%2f") {
		writeError(w, http.StatusBadRequest, pferr.Parsef("the request path is not allowed"))
		return
	}
	if err := h.checkAuth(r); err != nil {
		pe := pferr.Public(err, "unauthorized")
		if pe.Code == pferr.Timeout {
			writeError(w, http.StatusServiceUnavailable, pe)
		} else {
			writeError(w, http.StatusUnauthorized, pe)
		}
		return
	}

	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/repos/")
	if !ok || rest == "" {
		writeError(w, http.StatusNotFound, pferr.Needsf("Plainforge does not serve this path"))
		return
	}
	repoName, tail, ok := strings.Cut(rest, "/")
	if !ok || tail == "" {
		writeError(w, http.StatusNotFound, pferr.Needsf("Plainforge does not serve this path"))
		return
	}
	if err := store.CheckRepoName(repoName); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if h.LookupRepo == nil || h.Store == nil {
		writeError(w, http.StatusServiceUnavailable, pferr.Timeoutf("the forge store is not ready"))
		return
	}
	repo, found := h.LookupRepo(repoName)
	if !found {
		writeError(w, http.StatusNotFound, pferr.Needsf("this Plainforge has no repository with that name"))
		return
	}

	switch tail {
	case "tasks":
		switch r.Method {
		case http.MethodGet:
			h.handleFindTask(w, r, repo)
		case http.MethodPost:
			h.handleOpenTask(w, r, repo)
		default:
			writeError(w, http.StatusMethodNotAllowed, pferr.Parsef("that HTTP method is not used here"))
		}
	case "tasks/occurrence":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, pferr.Parsef("that HTTP method is not used here"))
			return
		}
		h.handleAddOccurrence(w, r, repo)
	case "changes":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, pferr.Parsef("that HTTP method is not used here"))
			return
		}
		h.handleProposeChange(w, r, repo)
	case "mirror":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, pferr.Parsef("that HTTP method is not used here"))
			return
		}
		h.handleSyncMirror(w, r, repo)
	default:
		writeError(w, http.StatusNotFound, pferr.Needsf("Plainforge does not serve this path"))
	}
}

func (h *Handler) handleFindTask(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	if r.URL.RawQuery != "" && strings.Contains(r.URL.RawQuery, ";") {
		writeError(w, http.StatusBadRequest, pferr.Parsef("the query string is malformed"))
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, pferr.Parsef("the query string is malformed"))
		return
	}
	for k, vals := range q {
		if (k != "key" && k != "open") || len(vals) != 1 {
			writeError(w, http.StatusBadRequest, pferr.Parsef("unexpected query parameter"))
			return
		}
	}
	if openVal := q.Get("open"); openVal != "" && openVal != "true" {
		writeError(w, http.StatusUnprocessableEntity, pferr.Needsf("only open=true task lookups are supported"))
		return
	}
	key := q.Get("key")
	task, ok, err := h.Store.FindOpenTask(repo, key)
	if err != nil {
		writeError(w, 0, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, FindResult{Found: false})
		return
	}
	writeJSON(w, http.StatusOK, FindResult{Found: true, Task: &task})
}

func (h *Handler) handleOpenTask(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	var draft TaskDraft
	if err := decodeJSON(r, 64<<10, &draft); err != nil {
		writeError(w, 0, err)
		return
	}
	task, err := h.Store.OpenTask(repo, draft)
	if err != nil {
		writeError(w, 0, err)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (h *Handler) handleAddOccurrence(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	var req OccurrenceRequest
	if err := decodeJSON(r, 4<<10, &req); err != nil {
		writeError(w, 0, err)
		return
	}
	task, err := h.Store.AddOccurrence(repo, req.ID)
	if err != nil {
		writeError(w, 0, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) handleProposeChange(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	var ch Change
	if err := decodeJSON(r, maxJSONRequest, &ch); err != nil {
		writeError(w, 0, err)
		return
	}
	res, err := h.Store.ProposeChange(repo, ch)
	if err != nil {
		pe := pferr.Public(err, "the change could not be proposed")
		if pe.Msg == "the branch already holds a different change" {
			writeError(w, http.StatusConflict, pe)
			return
		}
		writeError(w, 0, pe)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, res)
}

func (h *Handler) handleSyncMirror(w http.ResponseWriter, r *http.Request, repo *store.Repo) {
	var req MirrorRequest
	if err := decodeJSON(r, 4<<10, &req); err != nil {
		writeError(w, 0, err)
		return
	}
	syncSource, err := CheckMirrorContent(repo.Name(), req)
	if err != nil {
		writeError(w, 0, err)
		return
	}
	if syncSource && h.OnMirror != nil {
		h.OnMirror(repo)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Client calls Plainforge's Forge v1 HTTP endpoints.
type Client struct {
	BaseURL     string
	BearerToken string
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// NewClient returns a Forge v1 HTTP client for baseURL (for example
// "http://plainforge.clearedstack.svc.cluster.local:8080").
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Timeout: 10 * time.Second,
	}
}

func (c *Client) do(ctx context.Context, method, path string, reqBody, respBody any) (int, error) {
	if err := store.CheckRepoName("ok"); err != nil {
		return 0, err
	}
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, pferr.Parsef("the forge request could not be encoded")
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return 0, pferr.Parsef("the forge address is not a usable URL")
	}
	req.Header.Set("User-Agent", "plainforge-client")
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
	hc := c.HTTPClient
	if hc == nil {
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		hc = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, pferr.Timeoutf("Plainforge did not answer in time")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return resp.StatusCode, pferr.Timeoutf("Plainforge response could not be read in time")
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if respBody != nil {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(respBody); err != nil {
				return resp.StatusCode, pferr.Parsef("Plainforge returned malformed JSON")
			}
		}
		return resp.StatusCode, nil
	}
	var eb ErrorBody
	if err := json.Unmarshal(raw, &eb); err == nil && eb.Message != "" {
		switch eb.Code {
		case pferr.Timeout:
			return resp.StatusCode, pferr.Timeoutf("%s", eb.Message)
		case pferr.Parse:
			return resp.StatusCode, pferr.Parsef("%s", eb.Message)
		case pferr.NeedsSubtool:
			return resp.StatusCode, pferr.Needsf("%s", eb.Message)
		}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return resp.StatusCode, pferr.Timeoutf("Plainforge answered with status %d", resp.StatusCode)
	}
	return resp.StatusCode, pferr.Parsef("Plainforge answered with status %d", resp.StatusCode)
}

func checkClientRepo(repo string) error {
	if err := store.CheckRepoName(repo); err != nil {
		return pferr.Parsef("repository name is malformed")
	}
	return nil
}

// FindOpenTask looks up an open task in repo by key.
func (c *Client) FindOpenTask(ctx context.Context, repo, key string) (Task, bool, error) {
	if err := checkClientRepo(repo); err != nil {
		return Task{}, false, err
	}
	q := url.Values{}
	q.Set("key", key)
	q.Set("open", "true")
	var res FindResult
	if _, err := c.do(ctx, http.MethodGet, "/v1/repos/"+repo+"/tasks?"+q.Encode(), nil, &res); err != nil {
		return Task{}, false, err
	}
	if !res.Found || res.Task == nil {
		return Task{}, false, nil
	}
	return *res.Task, true, nil
}

// OpenTask creates a new open task in repo with occurrences=1.
func (c *Client) OpenTask(ctx context.Context, repo string, draft TaskDraft) (Task, error) {
	if err := checkClientRepo(repo); err != nil {
		return Task{}, err
	}
	var t Task
	if _, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/tasks", draft, &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// AddOccurrence increments an open task's occurrence count in repo.
func (c *Client) AddOccurrence(ctx context.Context, repo, taskID string) (Task, error) {
	if err := checkClientRepo(repo); err != nil {
		return Task{}, err
	}
	var t Task
	if _, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/tasks/occurrence", OccurrenceRequest{ID: taskID}, &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// ProposeChange commits a single-file replacement on change.Branch in change.Repo.
func (c *Client) ProposeChange(ctx context.Context, change Change) (bool, error) {
	if err := checkClientRepo(change.Repo); err != nil {
		return false, err
	}
	var res ChangeResult
	if _, err := c.do(ctx, http.MethodPost, "/v1/repos/"+change.Repo+"/changes", change, &res); err != nil {
		return false, err
	}
	return res.Created, nil
}

// SyncMirror requests synchronization of the named content kinds for repo.
func (c *Client) SyncMirror(ctx context.Context, repo string, content []string) error {
	if err := checkClientRepo(repo); err != nil {
		return err
	}
	_, err := c.do(ctx, http.MethodPost, "/v1/repos/"+repo+"/mirror", MirrorRequest{Repo: repo, Content: content}, nil)
	return err
}
