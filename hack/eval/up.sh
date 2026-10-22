#!/usr/bin/env bash
# Create the eval cluster and break it in one known way.
#
# This script mutates a cluster on purpose, which is the single exception to
# design principle 1. It is confined here, named so the read-only guard can
# recognize it, and it refuses to run against anything but its own kind cluster
# — an eval that can reach production is not an eval.
set -euo pipefail

cd "$(dirname "$0")"

FIXTURE="${1:-}"
CLUSTER="brief-eval"
CONTEXT="kind-${CLUSTER}"

if [ -z "$FIXTURE" ] || [ ! -d "fixtures/$FIXTURE" ]; then
  echo "usage: $0 <fixture>" >&2
  echo "available:" >&2
  for d in fixtures/*/; do echo "  $(basename "$d")" >&2; done
  exit 2
fi

for tool in kind kubectl; do
  command -v "$tool" >/dev/null || { echo "$tool is not installed" >&2; exit 1; }
done

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "==> creating kind cluster $CLUSTER"
  kind create cluster --config kind.yaml --wait 90s
fi

# Refuse to touch a cluster that is not the one this script created.
current=$(kubectl config current-context)
if [ "$current" != "$CONTEXT" ]; then
  echo "refusing to apply fixtures: current context is '$current', expected '$CONTEXT'" >&2
  exit 1
fi

echo "==> applying fixture $FIXTURE"
kubectl --context "$CONTEXT" apply -f "fixtures/$FIXTURE/manifest.yaml"

echo
echo "root cause this fixture encodes:"
sed 's/^/  /' "fixtures/$FIXTURE/expected.yaml"
