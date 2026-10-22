# Eval harness

A small set of deliberately broken clusters, each with its root cause known in
advance. From v0.1 on, every tool change is measured against these rather than
argued about.

```
make eval-up F=oom       # create the cluster, apply one fixture
make eval-down           # destroy it
```

Each fixture is two files: `manifest.yaml` breaks the cluster, `expected.yaml`
declares the answer. The eval scores two things, and the second is what
separates this project from a `kubectl` wrapper:

1. **Did the agent reach the right root cause?** `root_cause` is the reason code
   that must appear; `must_not_emit` lists the plausible wrong answers.
2. **What did it spend getting there?** `budget_tokens` and `max_tool_calls`
   against `kubectl_walk`, the hop sequence a human would have run.

## The fixtures

| Fixture | Root cause | Why this one |
| --- | --- | --- |
| `oom` | `CONTAINER_OOM_KILLED` | The baseline. Exit 137 means OOM, and the model should never have to infer that. |
| `image-not-found` | `IMAGE_NOT_FOUND` | Shares a symptom with a credentials failure and has the opposite remedy. |
| `pvc-unbound` | `PVC_UNBOUND` | The pod is Pending but the cause is two hops away. Reporting the pod is the failure mode. |
| `service-selector` | `SERVICE_HAS_NO_ENDPOINTS` | Nothing is in an error state. An agent that looks for failing pods calls this cluster healthy. |
| `node-tainted` | `POD_UNSCHEDULABLE` | Needs real scheduler predicates for the per-node breakdown, so its full answer is v0.2. |

## Safety

`up.sh` mutates a cluster, the one deliberate exception to design principle 1.
It is contained three ways: it only ever creates and targets its own kind
cluster, it aborts if the current context is not `kind-brief-eval`, and its path
is what the read-only guard in `.claude/` exempts. Nothing outside this directory
is allowed to write to a cluster.
