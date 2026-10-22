#!/usr/bin/env bash
# Destroy the eval cluster. Only ever the one this harness creates.
set -euo pipefail
CLUSTER="brief-eval"
command -v kind >/dev/null || { echo "kind is not installed" >&2; exit 1; }
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind delete cluster --name "$CLUSTER"
else
  echo "no cluster named $CLUSTER"
fi
