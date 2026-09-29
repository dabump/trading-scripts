#!/usr/bin/env bash
# Run the agent with the Alpaca credentials from .env.
#
# Any arguments are passed through to the agent, e.g.:
#   ./run.sh -verbose
#   ./run.sh -config config/other.yaml
set -euo pipefail

cd "$(dirname "$0")"

if [[ ! -f .env ]]; then
	echo "run.sh: .env not found — copy .env.example to .env and fill it in" >&2
	exit 1
fi

# Export every variable .env assigns, so the agent's os.Getenv sees them.
set -a
# shellcheck source=/dev/null
. ./.env
set +a

exec go run ./cmd/agent "$@"
