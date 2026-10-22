# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## State of the repository

**v0.1 feature-complete, unvalidated against a real cluster.** Both binaries build and the whole path is covered by an in-process end-to-end test; nothing has run against a live API server, because this machine has no kind, kubectl, helm or container runtime. The v0.1 gate — 5x fewer tokens than `kubectl` in three scenarios — is not measured yet, so the project's thesis is still unproven.

What exists: `internal/projection` (noise stripping plus per-kind profiles), `internal/cache` (controller-runtime informers, transform before store, plus a `Fake` every other package's tests use), `internal/analyzer` (the v0.1 reason codes), `internal/tools` (the three tools), `internal/rbac` and `internal/authz` (SubjectAccessReview with a TTL cache), `internal/server` (HTTP plus the audit log), `internal/shim` (MCP over stdio to HTTP), and `cmd/brief-server` plus `cmd/kubectl-brief`.

What does not: the Helm chart, the token benchmark, `logs_patterns`, and the projection's diff against schema defaults — v0.1 selects fields from a per-kind table in `internal/projection/project.go` instead, with a documented weak fallback for kinds it has no entry for.

**`README.md` is the single source of truth for the design.** A Spanish version of the same document and its PDF sit outside the repo, deliberately: they are historical, they are not canonical, and they are not kept in sync. Never reconcile the README against them or treat them as the spec — if they disagree, the README is right.

Consequences for working here:

- `README.md` is the specification. Changing behaviour means changing it too, especially the MCP surface.
- **The tools depend on `cache.Reader`, never on controller-runtime.** That interface is why the entire diagnostic layer is testable with no cluster, via `cache.Fake`. Keep new diagnostic code on the interface side of that line.
- `internal/e2e` runs a real call end to end in one process: MCP client, shim, HTTP, service, tool, analyzers, envelope. A change that breaks the contract shows up there first.

## Commands

```
make check              # vet + test — what CI runs
make test               # go test -race ./...
make test-one T=TestMarshalIsIdempotent P=./internal/envelope
make cover              # coverage.html
make fmt vet tidy
make bench-tokens       # the v0.1 gate: tokens against kubectl
make eval-up F=oom      # kind cluster + one broken fixture
make eval-down
```

`make help` lists them. The eval fixtures live in `hack/eval/fixtures/<name>/` as a `manifest.yaml` that breaks the cluster and an `expected.yaml` that declares the root cause, the reason code, the token budget and the `kubectl` walk being replaced.

## Guardrails configured in this repo

The seven principles below are prose, and prose is ignorable. Two of them are enforced mechanically:

- **`.claude/hooks/no-cluster-writes.sh`**, wired as a `PreToolUse` hook on `Bash` in `.claude/settings.json`, denies any `kubectl`/`oc`/`helm`/`kubeadm` command carrying a mutating verb — in any flag position, in any segment of a compound command, which is the gap plain deny rules leave open (`Bash(kubectl delete:*)` never matches `kubectl -n prod delete`). The deny list in the same file is a fast first layer over the common prefixes.
- Eval fixtures must break a cluster on purpose, so the guard exempts paths matching `hack/eval*`, `hack/fixtures*` and `make eval*`. Put deliberate breakage in a reviewed script there rather than typing it ad hoc — that keeps principle 1 enforceable instead of merely aspirational.
- **The `new-tool` skill** (`/new-tool`) carries the gates and the response contract for any change to the MCP surface. Invoke it when adding or reworking a tool rather than working from memory.

**Principles 3, 5 and 6 are now enforced by types, not by this file.** `envelope.Envelope` is the only way a tool returns data:

- `New` takes `as_of` and the watch lag as arguments and rejects a zero value, so there is no envelope without freshness.
- `Marshal` is the only serialization point and is where the ceiling is applied. Tools never call `json.Marshal` on their own output.
- `SetObjects` accepts `rbac.FilteredObjects` and nothing else. That type keeps its objects in an unexported field, so no package outside `internal/rbac` can put data in one — a tool physically cannot serialize data that skipped authorization.

Two consequences worth knowing before touching this code. Truncation drops log patterns, then events, then objects, then the least severe findings, and never `next_hints`; a response whose irreducible floor exceeds the budget is an error, not a silent overrun. And a response discloses only *how many* scopes RBAC withheld, never which — naming a namespace the caller may not read would itself be the disclosure principle 5 forbids. The full scopes go to the audit log via `WithheldScopes()`, for an operator who is authorized to see them.

Principle 1 is enforced by `.claude/`, and principle 2 by the absence of any generic tool: `list_objects` is the one escape hatch and is deliberately the least convenient to reach for.

What is still only prose: principles 4 and 7, which need human review.

## What `brief` is

A read-only MCP server that turns Kubernetes cluster state into answers an agent can afford to read: it answers a whole diagnostic question in one call, inside a token budget, instead of making the agent walk the object graph with `kubectl`. It is deliberately *not* a `kubectl` wrapper — it projects (strips noise and defaults), correlates (materializes implicit edges), pre-computes (does the arithmetic models get wrong), and remembers (records history so `what_changed` is answerable).

Naming is fixed across all artifacts: repo `brief`, plugin `kubectl brief`, Helm chart `brief`, API group `kubebrief.io`.

## Design principles that govern code decisions

These are the README's seven rules. When an implementation choice conflicts with one, the rule wins — they are not aspirations, they are the project's constraints:

1. **Read-only, no exceptions.** No tool mutates. The `ClusterRole` asks only for `get`, `list`, `watch`.
2. **Intent-level tools, not resource-level.** Expose diagnostic questions (`why_not_scheduled`, `what_changed`), never a generic `run_kubectl(cmd)`.
3. **Hard token budget per response.** On exhaustion: truncate, declare `truncated: true`, return a cursor. Never a silent cut.
4. **Everything deterministic is computed server-side.** Sums, scheduler predicates, diffs, owner correlation. The model gets facts, not inputs.
5. **The caller's RBAC rules.** Never return data the invoking user could not read with their own kubeconfig.
6. **Explicit freshness.** Every response carries `as_of` and watch lag.
7. **Diagnostics are data, not code.** A new analyzer is addable without recompiling or forking.

Principle 3 is the one extensions may not break: the base projection, the graph and the token budget are not configurable per analyzer.

## Architecture

Hybrid by necessity: the agent runs on the laptop (needs the user's identity), the cache runs in the cluster (needs to stay warm and record history). The API server is the only door between them, which is what makes the permission model work without inventing a credential system.

```
agent --MCP/stdio--> local shim --HTTPS, user's kubeconfig--> kube-apiserver --proxy--> in-cluster service
                                                                                        (informer cache)
```

- **Local shim** (single static binary, distributed via krew as `kubectl brief`): translates stdio↔HTTP, authenticates with the kubeconfig, multiplexes clusters. **Holds no diagnostic logic** — if it starts to, it has stopped being a shim.
- **In-cluster service** (Helm chart, 1-replica Deployment): all diagnostic logic and all state. Serves requests entirely from the in-memory informer cache; the only traffic it initiates is its own `watch` plus `SubjectAccessReview` calls. No internet access, enforced by a shipped `NetworkPolicy`.
- Transport is the API server service proxy (`/api/v1/namespaces/<ns>/services/<svc>:<port>/proxy/<path>`) — no Ingress, no certificate, no public DNS, and access is itself an RBAC permission (`services/proxy`).
- Authorization is **SubjectAccessReview**, not `impersonate`. Impersonation would require giving the pod a master key to the cluster. The cost accepted is hand-written filtering, granular at namespace+resource level, with a short-TTL answer cache so one tool call is not a hundred SARs.

The four components in the middle are the project's value — projection layer (`structured-merge-diff` + OpenAPI defaults, three profiles: `minimal`/`diagnostic`/`full`, listings never `full`), resource graph (`blast_radius`, `trace_path`), history recorder (in-memory ring buffer in v0.x, persistent backend optional in v1), analyzer engine (normalized reason codes, CEL for declarative analyzers).

Two implementation details decide cluster-scale viability: the informer `DefaultTransform` must drop `managedFields` **before** storing in cache, and cached GVKs must be opt-in with selectors — never `*`.

## Go is the stack for non-negotiable reasons

Four libraries exist only in Go and resolve most of the critical code: `structured-merge-diff` plus the API server OpenAPI (the projection layer), `client-go` informers with transforms (no real equivalent elsewhere), the `kube-scheduler` framework as a library (real predicates instead of a drifting approximation), and static binaries (the shim's install story). Python work — `drain3` log templating, anomaly detection, embeddings, the eval harness — lands later as an optional second service over gRPC, never mixed into the Go process.

Logging: an MCP server over stdio **must not log to stdout** — stdout is the protocol channel. Logs go to stderr. Tool-call audit logs are structured, to stdout only on the in-cluster service side.

## Roadmap gates

Phases close on verifiable criteria, not dates, and the order is not negotiable:

- **v0.1** — projection + cache, only `explain_workload`, `triage_cluster`, `list_objects`, plus the token benchmark. Gate: measures 5x fewer tokens than `kubectl` in 3 scenarios. If the savings do not appear, the thesis is wrong and the project should stop.
- **v0.2** — graph, `trace_path`, `blast_radius`, `why_not_scheduled`. Gate: one real incident resolved without `kubectl`.
- **v0.3** — history recorder and `what_changed`. Gate: two external users install it unaided.
- **v1.0** — CEL analyzers, SAR filtering, stable chart, krew plugin, docs.

Explicitly out of scope before v1: writes of any kind, multi-cluster aggregation, a bespoke UI, metrics integration beyond what resource analyzers need, the GitOps semantic index.

Evals from v0.1 on: kind clusters deliberately broken in known ways (OOM, bad image, unbound PVC, misspelled Service selector, tainted node), measuring both whether the agent reaches the right cause and how many tokens it spent.
