# Plainforge specification

> Lightweight MIT-licensed Go Git server in the clearedstack namespace acting as cluster-primary source of truth with push-mirroring to GitHub.

**Status: design.** This is a specification. It describes how Plainforge is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/plainforge/](https://clearedstack.com/platform/plainforge/). To change the specification, change it here and say why.

Plainforge (`v0.1.0-draft`, owned by Plainforge) is a clean-room, MIT-licensed Go Git server running in the `clearedstack` control-plane namespace. It replaces heavy third-party forges such as Forgejo, eliminating 40+ database tables and hundreds of megabytes of RAM, while serving as the cluster's primary source of truth for code and configuration.

## Overview & Role

ClearedStack treats the cluster as primary and GitHub as the public mirror:

- **Headless Primary Git Server:** Hosts bare `.git` repositories for `clearedstack.com`, all 10 deterministic workers (Trawl through Bellbuoy), sub-tools, and `app.yaml` deployment manifests.
- **Webhook & Mirror Trigger:** Whenever a commit lands on `main` or a new tag is created, Plainforge fires a push webhook to Keelworks and push-mirrors source commits and tags to GitHub.
- **Zero Web UI / Account Overhead:** Public browsing, status inspection, and anonymous `✎ Edit` proposals happen in Conn, so Plainforge needs no user accounts, issue UI, or PR web frontend.

## Runtime & Resource Budget

| Attribute | Value |
| --- | --- |
| **Namespace** | `clearedstack` (Control Plane) |
| **Kind** | `control-plane` |
| **Version** | `v0.1.0-draft` |
| **Owning Repo** | Plainforge (`plainforge`) |
| **RAM Request** | `64 MiB` |
| **Priority Class** | High (Control Plane, protected from eviction) |
| **Storage** | Persistent Volume Claim (bare `.git` repositories; 1 of 3 cluster PVCs) |
| **Network Exposure** | Cluster-private (`ClusterIP` only); outbound HTTPS push-mirror to GitHub |

## Interfaces & Data Contracts

| Peer / Target | Direction | Protocol / Mechanism | Data Contract |
| --- | --- | --- | --- |
| **Bellbuoy** | Inbound | Cluster-private Git / Patch API | Commits approved public edits (from Countersign) and opens fingerprinted `needs_subtool` tasks in owning repos |
| **Keelworks** | Outbound / Inbound | Push webhook + Git commit | Receives push webhooks on `main`/tag updates to start builds; commits updated `app.yaml` image digests tagged `[skip-ci]` |
| **Shipkeeper** | Inbound | Cluster-private Git read | Watches `app.yaml` manifests in Plainforge to reconcile deployments via Server-Side Apply |
| **GitHub** | Outbound | HTTPS Git push-mirror | Mirrors source commits and tags on change; **zero container images** are ever pushed to GitHub |

## Failure Modes & Recovery

- **GitHub Unreachable / Rate-Limited:** Because Plainforge is primary, internal commits, Keelworks builds, and Shipkeeper deployments continue operating normally even if outbound mirroring to GitHub is temporarily unreachable.
- **Build Loop Prevention:** Digest updates written back by Keelworks to `app.yaml` are tagged `[skip-ci]` so Plainforge does not re-trigger an infinite build loop.
- **Volume Loss / Corruption:** Plainforge holds one of the cluster's three Persistent Volume Claims. Nightly backups archive the bare repositories, encrypt them client-side with `age`, and transfer them off-node. The automated monthly restore drill mounts the restored bare repositories in an isolated scratch area and runs Git consistency checks to prove recoverability.
- **Pod Restart:** High Priority Class protects Plainforge from eviction; on restart, `k3s` reattaches the PVC with all bare repositories intact.

## Security & Clean-Room Boundaries

- **ClusterIP Only:** Plainforge has zero public Ingress and is unreachable from the open internet. Default-deny `NetworkPolicy` restricts cluster-internal access to authorized control-plane peers.
- **No Unapproved Public Commits:** Anonymous `✎ Edit` proposals cannot reach Plainforge directly; they must pass the 5-layer anti-spam shield in Muster and explicit human approval in Countersign before Bellbuoy writes a commit.
- **Source-Only Public Mirror:** Only source code commits and tags mirror to GitHub; signed OCI container images remain strictly internal inside Lazarette.
- **Clean-Room MIT Licensing:** Written from scratch in Go under the MIT license, with no upstream forge license or dependency baggage.
- **Unprivileged Execution:** Runs as non-root with dropped Linux capabilities and no privilege escalation.

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
- [Public edits and what stays private](https://clearedstack.com/platform/public-edits/)
