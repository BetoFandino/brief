#!/usr/bin/env bash
# PreToolUse/Bash guard for design principle 1: read-only, no exceptions.
#
# Deny permission rules match a fixed command prefix, so `kubectl -n prod delete
# pod x` walks straight past `Bash(kubectl delete:*)`. This splits the command on
# shell operators and, in every segment that invokes a cluster CLI, looks for a
# mutating verb as an exact token — wherever it sits among the flags.
#
# Eval fixtures have to break a cluster on purpose (see the Evals section of
# README.md). They do it through a reviewed script under hack/, not ad-hoc.
set -uo pipefail

cmd=$(jq -r '.tool_input.command // ""' 2>/dev/null) || exit 0
[ -n "$cmd" ] || exit 0

CLIS='kubectl oc helm kubeadm'
VERBS='apply create delete patch replace edit set scale autoscale annotate label
       taint cordon uncordon drain rollout run expose exec attach cp debug
       install upgrade uninstall rollback'

in_list() { local needle=$1 hay=$2 w; for w in $hay; do [ "$needle" = "$w" ] && return 0; done; return 1; }

# Deliberate fixture setup is exempt; it is reviewed as code, not typed ad-hoc.
case $cmd in *hack/eval*|*hack/fixtures*|*make\ eval*) exit 0 ;; esac

mapfile -t segments < <(printf '%s' "$cmd" | tr ';|&\n' '\n')

for seg in "${segments[@]}"; do
  read -r -a toks <<< "$seg"
  seen_cli=0
  for tok in "${toks[@]}"; do
    if [ "$seen_cli" -eq 0 ]; then
      in_list "${tok##*/}" "$CLIS" && seen_cli=1
      continue
    fi
    if in_list "$tok" "$VERBS"; then
      jq -n --arg v "$tok" '{
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason: ("brief is read-only (design principle 1, README.md): `" + $v + "` mutates the cluster. Read with get/describe/logs/events/top instead. Deliberate breakage for evals belongs in a fixture script under hack/.")
        }
      }'
      exit 0
    fi
  done
done

exit 0
