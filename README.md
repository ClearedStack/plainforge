<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Plainforge" height="56">
  </picture>
</p>

<p align="center">
  <strong>Holds the primary copy of every repository, announces each change and mirrors code outward.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: v0.1.0-draft (ready to deploy).** Plainforge serves bare Git repositories over smart HTTP, exposes the Forge v1 HTTP contract for Bellbuoy tasks and branch proposals, announces pushes to Keelworks with durable retry and projected ServiceAccount authentication, mirrors `main` and tags outward over HTTPS, and ships with `app.yaml`, `ko` build configuration, and hardened Kubernetes manifests (including `PriorityClass` and backup/restore-drill `CronJob`s). Remaining bootstrap items are listed in [Not built yet](#not-built-yet).

Plainforge is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Plainforge is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Plainforge does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`app.yaml`](app.yaml) | Desired application manifest read by Shipkeeper and pinned by Keelworks (`[skip-ci]`). |
| [`.ko.yaml`](.ko.yaml) | Build settings for `ko`: `CGO_ENABLED=0`, `linux/arm64`, distroless static base pinned by digest. |
| [`contract/v1/`](contract/v1) | The cluster-private Forge v1 HTTP contract (`forge.md`) and saved wire examples (`examples/forge_exchange.json`) for Bellbuoy. |
| [`deploy/`](deploy) | Kubernetes `PriorityClass`, `ServiceAccount`, `PersistentVolumeClaim`, `Service`, `Deployment`, `NetworkPolicy` and nightly `age` backup / monthly restore-drill `CronJob` manifests for namespace `clearedstack`. |
| [`brand/`](brand) | The Plainforge logo and mark, for light and dark backgrounds. |
| [`cmd/plainforge/`](cmd/plainforge) | The program. |
| [`internal/`](internal) | Its parts: `pferr` (the three error codes), `pkt` (Git's line format), `store` (repositories and the multi-repository catalog on disk), `pack` (Git packs), `forge` (Bellbuoy task ledger, deterministic branch splicing and `/v1/repos/...` HTTP handler/client), `server` (the HTTP service, `/healthz` and the push rules), `notify` (the Keelworks push announcement and on-disk retry spool), `mirror` (outbound smart HTTP push-mirror and retry queue). |
| [`testdata/make-fixtures.sh`](testdata/make-fixtures.sh) | Rebuilds the saved example packs with an ordinary Git. The tests do not run it. |

## What works today

Plainforge is written in Go with the standard library only (`module github.com/ClearedStack/plainforge`); it has no external dependencies and does not call the `git` program.

- **One or many bare repositories over smart HTTP.** Serve a directory of `<name>.git` repositories with `-repos` (creating named repositories on first start with `-init-repos`) or a single repository with `-repo`. `git clone`, `git fetch`, `git pull`, `git ls-remote` and `git push` from an ordinary Git client work against `/<name>.git`.
- **Health endpoint.** `GET /healthz` returns `200` with `{"status":"ok"}` for Kubernetes liveness and readiness probes.
- **Bellbuoy Forge v1 HTTP API (`/v1/repos/{repo}/...`).** Implements [`contract/v1/forge.md`](contract/v1/forge.md) matching Bellbuoy's five `Forge` operations: `FindOpenTask`, `OpenTask`, `AddOccurrence`, `ProposeChange` (deterministically splicing single-file `DATA`/`CONFIG` changes onto `refs/heads/edit/...` via Plainforge's quarantine without touching `main`), and `SyncMirror` (`"source"` and `"task"`). Tasks and change proposals persist atomically on disk across restarts, with optional bearer token enforcement via `-forge-token-file` (`PLAINFORGE_FORGE_TOKEN_FILE`).
- **Git's own loose layout and quarantine.** Each repository is kept on disk as loose objects and one file per ref, so `git fsck --strict` can check it. Startup sweeps left-over staging directories before serving. Every push is unpacked into a holding area first and reaches the repository only after every object has been rebuilt, hashed and checked; a push that fails any check leaves nothing behind. Concurrent pushes to the same repository are serialized cleanly.
- **Ref update rules.** A ref update is accepted only if a rule allows it:
  - only branches (`refs/heads/...`) and tags (`refs/tags/...`), with names made of letters, digits, `.`, `_`, `-` and `/`;
  - the old value the client sends must be the value the ref has now;
  - a branch must name a commit and may only move forward (fast-forward);
  - a tag may be created, on a commit or a tag object, and never moved;
  - nothing can be deleted.
- **Keelworks push notifications with bearer token and durable spool.** When an update is accepted, Plainforge writes a notification record to disk and `POST`s it to `-notify-url`:
  - for every change of `refs/heads/main`, unless the message of the new tip commit contains `[skip-ci]`;
  - for every new tag;
  - for nothing else.

  The body is one line of JSON matching Keelworks' `contract/v1/push.md`:

  ```json
  {"repository":"example","ref":"refs/heads/main","before":"<40 hex digits>","after":"<40 hex digits>"}
  ```

  When `-notify-token-file` is set, Plainforge reads the projected Kubernetes `ServiceAccount` token on each send (so rotated tokens take effect without restarting) and sends `Authorization: Bearer <token>`. Transient failures (`429`, `502`, `503`, `504` and network timeouts) stay in the on-disk spool and are retried in order across restarts without blocking the Git push.
- **Outbound HTTPS push-mirror.** When `-mirror-url` (or `PLAINFORGE_MIRROR_URL`) is set, accepted updates to `refs/heads/main` and `refs/tags/*` are queued on disk and pushed outward over Git's smart HTTP `git-receive-pack` protocol using HTTPS and optional `-mirror-token-file` (`x-access-token` basic auth, refused over plain HTTP). Non-`main` branches and local notes never leave the cluster. Rate limits (`429`) and transient `5xx`/network failures are retried in the background while incoming pushes continue inside the cluster.
- **Three public error codes.** Every refusal uses `E_TIMEOUT`, `E_PARSE` or `E_NEEDS_SUBTOOL` and a fixed sentence. No error or log line contains a host name, an address, a path or a credential.

### Run it

You need Go 1.24 or newer.

Serve a catalog of repositories:

```sh
go build -o plainforge ./cmd/plainforge
./plainforge \
  -repos /tmp/plainforge-repos \
  -init-repos clearedstack.com,plainforge,trawl \
  -listen 127.0.0.1:8080 \
  -notify-url http://127.0.0.1:8080/webhook
```

Or serve a single repository:

```sh
./plainforge -repo /tmp/example.git -listen 127.0.0.1:8080
```

`-notify-url`, `-notify-token-file`, `-mirror-url` (`PLAINFORGE_MIRROR_URL`), `-mirror-token-file` (`PLAINFORGE_MIRROR_TOKEN_FILE`) and `-forge-token-file` (`PLAINFORGE_FORGE_TOKEN_FILE`) are optional. In another terminal:

```sh
git clone http://127.0.0.1:8080/trawl.git
cd trawl
echo "hello" > README.txt && git add README.txt && git commit -m "First commit"
git push origin HEAD:main
```

### Test it

```sh
go vet ./...
go test -race ./...
```

The tests need no network. Each rule has saved examples in a `testdata/` or `contract/` folder: [`internal/server/testdata/exchange.json`](internal/server/testdata/exchange.json) replays a full smart HTTP conversation byte for byte, [`contract/v1/examples/forge_exchange.json`](contract/v1/examples/forge_exchange.json) replays the Forge v1 task/change/mirror contract byte for byte, [`internal/notify/testdata/keelworks_contract.json`](internal/notify/testdata/keelworks_contract.json) checks Keelworks webhook payloads and status handling, and [`internal/mirror/testdata/rules.json`](internal/mirror/testdata/rules.json) checks outbound mirror ref filtering and status parsing. Integration tests also drive the real `git` program against multi-repository, branch-proposal, notification and mirror servers when `git` is installed.

### Limits of what is built

- Plainforge reads only loose-object repositories it wrote itself. A repository with packed objects, `packed-refs`, alternates or SHA-256 object names is refused at start-up; do not run `git gc` on its volume.
- It never packs or deletes objects, so repositories only grow on disk.
- No object may be larger than 8 MiB, no push larger than 256 MiB, and no request may take longer than two minutes. With `GOMEMLIMIT` defaulting to `48MiB`, pushes and fetches stream within the `64Mi` container memory limit.
- Shallow clones (`--depth`), partial clones, protocol version 2, client-requested side-band progress on push, atomic pushes and signed pushes are refused or not offered. Git falls back by itself where it can.
- A fetch sends whole objects rather than deltas and walks full commit ancestry to compute missing objects.
- Plainforge enforces network isolation through Kubernetes `NetworkPolicy` (with optional `-forge-token-file` bearer auth on `/v1/`), matching `SPEC.md` ("no user accounts").
- Cloning an empty repository leaves the client on Git's own default branch name until the first commit is pushed to `main`.

## Not built yet

Items from [`SPEC.md`](SPEC.md) and [`component.json`](component.json) that stay outside the Go binary or wait on cluster bootstrap:

- **Git over SSH.** Mentioned in `component.json` boundaries and omitted from the `SPEC.md` interface table. Plainforge has no user accounts or SSH key store, Go's standard library has no SSH server, and every cluster peer uses smart HTTP.
- **Bootstrap image digests and backup helper image.** [`app.yaml`](app.yaml), [`deploy/plainforge.yaml`](deploy/plainforge.yaml), and [`deploy/backup-cronjob.yaml`](deploy/backup-cronjob.yaml) pin `sha256:0000...` bootstrap placeholders until Lazarette and Keelworks publish the first signed `plainforge` and `plainforge-backup` images and cluster `age` backup keys are provisioned.

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `cs-git` |
| **Namespace** | `clearedstack` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/plainforge` |
| **Memory budget** | 64 MiB |
| **Depends on** | Nothing else in the platform |
| **Depended on by** | [Keelworks](https://github.com/ClearedStack/keelworks), [Shipkeeper](https://github.com/ClearedStack/shipkeeper), [Moldloft](https://github.com/ClearedStack/moldloft), [Chartroom](https://github.com/ClearedStack/chartroom), [Bellbuoy](https://github.com/ClearedStack/bellbuoy) |

### Boundaries

- Git over HTTP and SSH, inside the cluster only
- Push notifications to Keelworks
- Outbound mirror of code to GitHub
- Commits from Bellbuoy, Keelworks and Moldloft; reads by Shipkeeper

## Names

`plainforge` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
