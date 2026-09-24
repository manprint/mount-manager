#!/usr/bin/env bash
# Runs the privileged e2e scenarios. Invoke as: sudo -n /mnt/fabio/dati/Git/SperimentazioniAI/mount-manager/e2e-root.sh [test flags]
set -euo pipefail
cd "$(dirname "$(readlink -f "$0")")"
if [[ ${EUID} -ne 0 ]]; then
  echo "must run as root: sudo -n $(pwd)/e2e-root.sh" >&2
  exit 2
fi
if [[ ! -x bin/mount-manager || ! -x bin/e2e.test ]]; then
  echo "build first: go build -o bin/mount-manager ./cmd/mount-manager && go test -c -tags e2e -o bin/e2e.test ./test/e2e" >&2
  exit 2
fi
exec env MM_BIN="$(pwd)/bin/mount-manager" ./bin/e2e.test -test.v -test.count=1 -test.timeout=45m "$@"
