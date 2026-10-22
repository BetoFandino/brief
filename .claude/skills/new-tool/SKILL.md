---
name: new-tool
description: Add or change a tool on brief's MCP surface, holding the response contract and the seven design principles. Use when adding a tool, renaming one, changing a tool's inputs or outputs, adding a reason code, or reworking the tool table in README.md.
---

# Adding a tool to the MCP surface

The tool contract is the highest-impact, lowest-cost-of-change part of the design, and the part expected to be rewritten several times. That is exactly why it needs a checklist: invariants get lost in rewrites, not in first drafts.

Work through the four gates in order. A tool that fails a gate does not get reworded until it passes — it gets dropped or merged into an existing tool.

## Gate 1 — Is this a question, or a verb?

Principle 2: the surface exposes diagnostic questions, not API verbs.

Say the tool's name out loud as a question an operator would actually ask during an incident. `why_not_scheduled` → "why won't this pod start?" Good. A name that reads as `<verb>_<resource>` is an API call wearing a costume, and hands the model the walk that brief exists to perform.

The one sanctioned exception already exists: `list_objects`, the escape hatch, deliberately the least convenient tool to reach for. Do not add a second one.

## Gate 2 — Does it replace a walk?

The value of a tool is the hops it collapses. Write down the `kubectl` sequence a human would run to answer the same question. If that sequence is one or two hops, this is not a tool — it is `list_objects` with a selector, or a field that belongs in an existing tool's response.

If it is six to ten hops, state which ones, because that number is the benchmark target and goes in the eval.

## Gate 3 — Which milestone owns it?

The roadmap order is not negotiable, and each phase closes on a verifiable criterion:

- **v0.1** `explain_workload`, `triage_cluster`, `list_objects` — projection and cache only.
- **v0.2** needs the graph: `trace_path`, `blast_radius`, `why_not_scheduled`.
- **v0.3** needs history: `what_changed`.
- **v1.0** needs CEL analyzers and SAR filtering.

A tool that depends on a layer the current milestone has not built is a scope leak, which is the named risk most likely to sink a solo-maintainer project. Note the dependency and stop.

## Gate 4 — What in it is deterministic?

Principle 4: everything deterministic is computed server-side. List every number, comparison, sum, predicate and diff the answer requires, and confirm each is computed before serialization. A fact miscalculated by the model is indistinguishable from a correct one — that is the whole reason this layer exists.

If the answer is "the model can work it out from the fields," the tool is not finished.

## The response contract

Every response, from every tool, carries all of this. There is no per-tool exemption:

- [ ] **`as_of`** plus watch lag (`lag_ms`). Principle 6 — the cache lies for a few hundred milliseconds and the agent has to know.
- [ ] **A declared token budget**, enforced at serialization. On exhaustion: `truncated: true`, a summary of what was omitted, and a `cursor`. Never a silent cut mid-JSON.
- [ ] **Stable IDs** in `ns/kind/name` form, reusable verbatim as another tool's input.
- [ ] **Normalized reason codes** (`CONTAINER_OOM_KILLED`), never free prose. The server emits codes; the model writes the sentences.
- [ ] **`next_hints`** — at most three, with arguments pre-filled. The cheapest way to steer the agent's walk without pushing that logic into the user's prompt.
- [ ] **Compact JSON** for anything that lists. YAML only when inspecting one object.
- [ ] **An output profile** — `minimal`, `diagnostic` or `full`. A listing never uses `full`.
- [ ] **RBAC filtering** before serialization, in `sar` mode. Principle 5 — never return what the caller could not read with their own kubeconfig.

Principles 3, 5 and 6 should hold because of the type the handler returns, not because this list says so. If a handler can serialize a response without an `as_of`, without a budget, or holding data that never passed the SAR filter, fix that envelope type rather than adding a reminder here.

## Registering it

1. **`README.md` is the single source of truth** for the surface. Add or amend the row in the v1 tools table, matching the existing column voice: what it answers, main input, output. The Spanish document and PDF outside the repo are historical and are not updated.
2. If a tool registry file exists, add the entry there too — a handler that is registered but undeclared must fail a test, so adding a tool always means touching the contract.
3. Add its reason codes to the normalized code list.
4. Add an eval fixture: a kind cluster broken in a way this tool is the right answer to, with the root cause known in advance. The eval measures both whether the agent reaches that cause and how many tokens it spent. Without it, every later change to this tool is a blind bet.

## What a new tool may never do

The base projection, the resource graph and the token budget are not configurable per tool or per analyzer. A tool may ask for more graph context; it may not bypass the response ceiling, and it may not return raw Kubernetes objects. If an extension can break principle 3, principle 3 does not exist.

And no tool mutates anything, in any mode, for any reason. The `ClusterRole` asks for `get`, `list` and `watch` — adding a verb to it is not a tool change, it is the end of the project's adoptability.
