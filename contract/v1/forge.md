# Plainforge Forge HTTP Contract (v1)

Plainforge exposes a small, deterministic, cluster-private JSON HTTP contract under `/v1/repos/{repo}/...` on port `8080` so Bellbuoy (`cs-workers`) can manage fingerprinted failure tasks, owner-approved Portal `ISSUE` tasks, single-file `DATA`/`CONFIG` branch proposals, and outbound mirror synchronization without external forge software.

This contract matches the five operations of Bellbuoy's `Forge` interface (`FindOpenTask`, `OpenTask`, `AddOccurrence`, `ProposeChange`, `SyncMirror`).

---

## Transport and Authentication

- **Base URL in-cluster:** `http://plainforge.clearedstack.svc.cluster.local:8080`
- **Network boundary:** Cluster-private `ClusterIP` only. Kubernetes `NetworkPolicy` restricts TCP `8080` ingress to authorized peers in `clearedstack` and `cs-workers`.
- **Optional bearer authentication:** When Plainforge is started with `-forge-token-file` (`PLAINFORGE_FORGE_TOKEN_FILE`), every `/v1/...` request must supply `Authorization: Bearer <token>`. Missing or mismatched credentials return `401` with `E_PARSE`.
- **Content type:** Every `POST` request must send `Content-Type: application/json` with a single JSON object and no unknown fields. Every `/v1/...` response returns `Content-Type: application/json` ending in `\n`.

---

## Error Envelope and Public Codes

Every non-2xx `/v1/...` response returns one JSON object:

```json
{"code":"E_PARSE","message":"the task key is missing or malformed"}
```

Only the three platform error codes are ever returned:

| Code | HTTP Status | Meaning |
| --- | --- | --- |
| `E_PARSE` | `400`, `401`, `405`, `409` | Malformed JSON, missing/invalid field, invalid bearer token, wrong HTTP method, closed/unknown task on occurrence update, or conflicting change on an existing branch. Do not retry unchanged. |
| `E_NEEDS_SUBTOOL` | `404`, `422` | Valid request naming an unknown repository, an unsupported route/filter, or a content kind (`image`, `secret`, `capture`, ...) that may not leave for the public mirror. Pause and escalate. |
| `E_TIMEOUT` | `503`, `504` | Temporary storage or I/O failure while reading or flushing the repository ledger. Safe to retry with backoff. |

No response or log line ever includes a hostname, IP address, filesystem path, or credential.

---

## Repository Parameter (`{repo}`)

`{repo}` in `/v1/repos/{repo}/...` must match `^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`, contain no `..`, and not end in `.git` (for example `trawl` or `clearedstack.com`). A malformed `{repo}` returns `400 E_PARSE`; an unknown repository returns `404 E_NEEDS_SUBTOOL`.

---

## Endpoints

### 1. Find Open Task

`GET /v1/repos/{repo}/tasks?key={key}&open=true`

Looks up an open task in `{repo}` with exact `key` (`1..256` printable UTF-8 bytes, no control characters). `open` is optional and defaults to `true`.

- **When found (`200 OK`):**
  ```json
  {"found":true,"task":{"id":"trawl#1","repo":"trawl","key":"fp-abc123","title":"trawl: E_PARSE [fp-abc12]","body":"Worker: trawl","occurrences":1,"open":true}}
  ```
- **When no open task has `key` (`200 OK`):**
  ```json
  {"found":false}
  ```

### 2. Open Task

`POST /v1/repos/{repo}/tasks`

Opens a new task in `{repo}` with `occurrences: 1` and `open: true`, assigning sequential identifier `<repo>#<n>` (`1, 2, ...`).

- **Request (`<= 64 KiB`):**
  ```json
  {"key":"fp-abc123","title":"trawl: E_PARSE [fp-abc12]","body":"Worker: trawl\nError code: E_PARSE"}
  ```
  - `key`: required, `1..256` single-line printable UTF-8 bytes.
  - `title`: required, `1..512` single-line printable UTF-8 bytes.
  - `body`: `0..32768` UTF-8 plain-text bytes (`\n` and `\t` allowed; no `\r` or control characters).
- **Response (`201 Created`):**
  ```json
  {"id":"trawl#1","repo":"trawl","key":"fp-abc123","title":"trawl: E_PARSE [fp-abc12]","body":"Worker: trawl\nError code: E_PARSE","occurrences":1,"open":true}
  ```

### 3. Add Occurrence

`POST /v1/repos/{repo}/tasks/occurrence`

Increments `occurrences` by `1` on an open task in `{repo}`. Passing the task ID in the JSON body avoids `#` URL-fragment truncation on `<repo>#<n>` identifiers.

- **Request (`<= 4 KiB`):**
  ```json
  {"id":"trawl#1"}
  ```
- **Response (`200 OK`):**
  ```json
  {"id":"trawl#1","repo":"trawl","key":"fp-abc123","title":"trawl: E_PARSE [fp-abc12]","body":"Worker: trawl\nError code: E_PARSE","occurrences":2,"open":true}
  ```
- **If the task does not exist or is closed (`400 Bad Request`):**
  ```json
  {"code":"E_PARSE","message":"the task to add to is not open"}
  ```

### 4. Propose Change

`POST /v1/repos/{repo}/changes`

Replaces one UTF-8 plain-text file on `refs/heads/{branch}` (starting from the current `refs/heads/main` tree when `main` exists, or from an empty tree otherwise) and records the merge proposal. Plainforge never writes to `refs/heads/main` through this endpoint.

- **Request (`<= 256 KiB`):**
  ```json
  {
    "repo": "trawl",
    "branch": "edit/EP-0001",
    "path": "config/sources.json",
    "content": "{\n  \"enabled\": true\n}\n",
    "title": "Portal proposal EP-0001: sources",
    "message": "Portal proposal EP-0001: sources\n\nClass: CONFIG\nTarget: sources"
  }
  ```
  - `repo`: optional; when non-empty, must equal `{repo}`.
  - `branch`: required valid Git branch name other than `main` (for example `edit/EP-0001`), without a `refs/heads/` prefix.
  - `path`: required clean relative file path (`1..512` bytes, no leading/trailing `/`, no `.`, `..`, or `.git` segments).
  - `content`: UTF-8 plain text (`0..131072` bytes, no `\r` or `\x00`).
  - `title`: required single-line UTF-8 (`1..512` bytes).
  - `message`: required UTF-8 plain text (`1..16384` bytes).
- **First proposal on `branch` (`201 Created`):**
  ```json
  {"created":true,"branch":"edit/EP-0001","commit":"<40-hex-sha1>"}
  ```
- **Idempotent replay of the exact same `Change` (`200 OK`):**
  ```json
  {"created":false,"branch":"edit/EP-0001","commit":"<40-hex-sha1>"}
  ```
- **Different change on an already-used `branch` (`409 Conflict`):**
  ```json
  {"code":"E_PARSE","message":"the branch already holds a different change"}
  ```

### 5. Sync Mirror

`POST /v1/repos/{repo}/mirror`

Requests synchronization of `{repo}` for the listed content kinds.

- **Request (`<= 4 KiB`):**
  ```json
  {"repo":"trawl","content":["source"]}
  ```
  - `repo`: optional; when non-empty, must equal `{repo}`.
  - `content`: non-empty array of content kind strings:
    - `"source"`: enqueues `{repo}` on Plainforge's outbound HTTPS Git push-mirror (`refs/heads/main` and `refs/tags/*`).
    - `"task"`: acknowledged (`200 OK`); tasks remain stored in Plainforge's cluster-private ledger per `SPEC.md` ("Source-Only Public Mirror").
    - Any other `[a-z][a-z0-9_-]{0,31}` kind (such as `"image"`, `"secret"`, `"capture"`): refused with `422 E_NEEDS_SUBTOOL`.
    - Malformed or empty strings: refused with `400 E_PARSE`.
- **Response (`200 OK`):**
  ```json
  {"status":"ok"}
  ```
