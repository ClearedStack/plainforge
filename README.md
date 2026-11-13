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

> **Status: design.** The specification is not built. One first milestone is: Plainforge can serve a single repository to an ordinary Git client over HTTP and announce pushes. Everything else in [`SPEC.md`](SPEC.md) is still to do; see [What works today](#what-works-today) and [Not built yet](#not-built-yet).

Plainforge is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Plainforge is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Plainforge does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Plainforge logo and mark, for light and dark backgrounds. |
| [`cmd/plainforge/`](cmd/plainforge) | The program. |
| [`internal/`](internal) | Its parts: `pferr` (the three error codes), `pkt` (Git's line format), `store` (the repository on disk), `pack` (Git packs), `server` (the HTTP service and the push rules), `notify` (the push announcement). Each has a `testdata/` folder with its saved examples. |
| [`testdata/make-fixtures.sh`](testdata/make-fixtures.sh) | Rebuilds the saved example packs with an ordinary Git. The tests do not run it. |

## What works today

One milestone of the specification is built: **one bare repository, served over Git's smart HTTP protocol, with a notification when a push lands.** It is written in Go with the standard library only; it has no dependencies and does not call the `git` program.

- `git clone`, `git fetch`, `git pull`, `git ls-remote` and `git push` from an ordinary Git client work against it.
- The repository is kept on disk in Git's own layout (loose objects and one file per ref), so `git fsck` can check it. A missing or empty directory is set up as a new repository whose default branch is `main`.
- A push is unpacked into a holding area first. It reaches the repository only after every object has been rebuilt, hashed and checked: each commit, tree and tag must be well formed, and everything it refers to must be present and of the right kind. A push that fails any check leaves nothing behind.
- A ref update is accepted only if a rule allows it:
  - only branches (`refs/heads/…`) and tags (`refs/tags/…`), with names made of letters, digits, `.`, `_`, `-` and `/`;
  - the old value the client sends must be the value the ref has now;
  - a branch must name a commit and may only move forward (fast-forward);
  - a tag may be created, on a commit or a tag object, and never moved;
  - nothing can be deleted.
- When an update is accepted, Plainforge sends one HTTP `POST` to the address given with `-notify-url`:
  - for every change of `refs/heads/main`, unless the message of the new tip commit contains `[skip-ci]`;
  - for every new tag;
  - for nothing else.

  The body is one line of JSON, always in this form:

  ```json
  {"repository":"example","ref":"refs/heads/main","before":"<40 hex digits>","after":"<40 hex digits>"}
  ```

  It is tried once, with a five second limit. If it fails, the push still lands and the failure is logged.
- Anything else is refused with one of the three public error codes, `E_TIMEOUT`, `E_PARSE` or `E_NEEDS_SUBTOOL`, and a fixed sentence. Git shows the sentence to the person who pushed. No error or log line contains a host name, an address, a path or a credential.

### Run it

You need Go 1.24 or newer.

```sh
go build -o plainforge ./cmd/plainforge
./plainforge -repo /tmp/example.git -listen 127.0.0.1:8080 -notify-url http://127.0.0.1:9000/hook
```

`-repo` is the directory of the one repository; its name, without `.git`, is the name in the URL. `-notify-url` may be left out, and then nothing is announced. In another terminal:

```sh
git clone http://127.0.0.1:8080/example.git
cd example
echo "hello" > README.txt && git add README.txt && git commit -m "First commit"
git push origin HEAD:main
```

### Test it

```sh
go test ./...
```

The tests need no network. Each rule has saved examples in a `testdata/` folder next to the code: an input and the exact output expected. [`internal/server/testdata/exchange.json`](internal/server/testdata/exchange.json) is a whole saved conversation (pushes, fetches and refusals) that is replayed and compared byte for byte. Some tests also drive the real `git` program against the server; if `git` is not installed those tests say they were skipped.

### Limits of what is built

These are choices made for the milestone. The specification does not state them; each is raised as a question in the pull request that added this code.

- It reads only repositories it wrote itself. A repository with packed objects, `packed-refs`, alternates or SHA-256 object names is refused at start-up. Running `git gc` on the volume would therefore stop Plainforge from starting.
- It never packs or deletes anything, so the repository only grows.
- No object may be larger than 8 MiB, no push larger than 256 MiB, and no request may take longer than two minutes.
- Shallow clones (`--depth`), partial clones, protocol version 2, side-band progress messages, atomic pushes and signed pushes are refused or not offered. Git falls back by itself where it can.
- A fetch sends whole objects, not deltas, and works out what the client already has by walking the full history. That is correct and slow for large repositories.
- There is no access control. Anything that can reach the port can read and push.
- Cloning an empty repository leaves the client on Git's own default branch name, not on `main`.

## Not built yet

Everything in [`SPEC.md`](SPEC.md) beyond the milestone:

- more than one repository;
- Git over SSH;
- the push-mirror to GitHub;
- the patch interface for Bellbuoy and its `needs_subtool` tasks;
- retrying or queueing a notification that failed;
- the container image built with `ko`, the Kubernetes manifests, the `NetworkPolicy` and the priority class;
- backups with `age` and the monthly restore drill;
- a measurement inside a container limited to 64 MiB with a read-only root filesystem. Outside a container, pushing and then cloning about 20 MiB of made-up files peaked at about 24 MiB of memory.

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
