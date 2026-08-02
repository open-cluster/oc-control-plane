#!/usr/bin/env bash
#
# Run something against a live model provider.
#
# There are two ways to point this product at a real model and they take DIFFERENT configuration,
# which is the thing this script exists to make obvious:
#
#   scenario    one red-herring investigation, configured by FLAGS on cmd/redherring.
#               Provisions nothing, reaches no cluster, serves pre-baked evidence. This is the
#               instrument for judging whether the prompt and schemas actually work.
#
#   controlplane  the real control plane, configured by ENVIRONMENT VARIABLES. It investigates
#               real workloads through a real Relay, and needs a database and a Relay to do it.
#
# The credential is read from a FILE in both cases and never from an environment value, because an
# environment value is readable from a process listing and appears in every diagnostic dump of the
# environment. Keep it under .secrets/, which is gitignored.
#
# Usage:
#   scripts/live-model.sh scenario [provider] [model]
#   scripts/live-model.sh controlplane
#   scripts/live-model.sh config
#
set -euo pipefail

cd "$(dirname "$0")/.."

# Local configuration, if there is any. It holds PATHS rather than secrets — every credential in
# this product is a file the configuration names — so sourcing it puts no key in the environment.
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

# Where credentials live. One file per provider, holding the key and nothing else.
SECRETS_DIR="${OC_SECRETS_DIR:-.secrets}"
ZAI_KEY="${SECRETS_DIR}/zai.key"
ANTHROPIC_KEY="${SECRETS_DIR}/anthropic.key"

# Which deployment answers by default. Both are priced in the shipped rate table; a model that is
# not in it is refused at startup rather than costed at zero.
DEFAULT_PROVIDER="${OC_MODEL_PROVIDER:-zai}"
DEFAULT_MODEL="${OC_MODEL_NAME:-glm-5}"

# How hard to think. This is the primary cost and latency lever, and which value is right is an
# empirical question — which is why it is configuration rather than a constant.
EFFORT="${OC_MODEL_EFFORT:-high}"

# A single call may take minutes on a model that thinks before answering. A per-call timeout that
# fires on a working provider reports an outage that never happened.
REQUEST_TIMEOUT="${OC_MODEL_REQUEST_TIMEOUT:-5m}"
ROUND_DEADLINE="${OC_MODEL_ROUND_DEADLINE:-15m}"

key_file_for() {
  case "$1" in
    zai) echo "${ZAI_KEY}" ;;
    anthropic) echo "${ANTHROPIC_KEY}" ;;
    *) echo "unknown provider: $1" >&2; exit 2 ;;
  esac
}

case "${1:-scenario}" in

  scenario)
    provider="${2:-${DEFAULT_PROVIDER}}"
    model="${3:-${DEFAULT_MODEL}}"
    key="$(key_file_for "${provider}")"
    if [[ ! -s "${key}" ]]; then
      echo "no credential at ${key}." >&2
      echo "write the key to that file — one line, nothing else — and run this again." >&2
      exit 1
    fi

    stamp="$(date +%Y%m%dT%H%M%S)"
    transcript="${SECRETS_DIR}/redherring-${provider}-${model}-${stamp}.json"

    echo "Running ONE red-herring investigation against ${provider}/${model}."
    echo "This calls a paid API. Every invocation is another paid run."
    echo

    exec go run ./cmd/redherring \
      -provider "${provider}" \
      -model "${model}" \
      -key-file "${key}" \
      -effort "${EFFORT}" \
      -transcript "${transcript}" \
      -deadline "${ROUND_DEADLINE}" \
      -request-timeout "${REQUEST_TIMEOUT}"
    ;;

  controlplane)
    provider="${DEFAULT_PROVIDER}"
    key="$(key_file_for "${provider}")"
    if [[ ! -s "${key}" ]]; then
      echo "no credential at ${key}" >&2
      exit 1
    fi

    # The model deployment. Nothing here carries a secret: the key is a PATH.
    export OC_MODEL_PROVIDER="${provider}"
    export OC_MODEL_NAME="${DEFAULT_MODEL}"
    export OC_MODEL_KEY_FILE="${key}"
    export OC_MODEL_EFFORT="${EFFORT}"

    # Consent is per provider and nothing listed permits nothing, so a configured provider that is
    # not named here is a refusal to START rather than a round that fails later. The question a
    # customer answers is about a subprocessor, which is why this is not implied by configuring one.
    export OC_MODEL_CONSENTED_PROVIDERS="${OC_MODEL_CONSENTED_PROVIDERS:-${provider}}"

    # A spending limit across rounds, in micro-cents. Unset means no ceiling, which is an operator
    # fact and not a currency. 500000000 is about $5.
    export OC_MODEL_COST_CEILING_MICROCENTS="${OC_MODEL_COST_CEILING_MICROCENTS:-500000000}"

    # Everything below is the control plane's existing configuration and is NOT about the model.
    # It is listed so that a first run fails on something nameable rather than on a nil pool.
    : "${OC_HTTP_ADDRESS:?set it, for example 127.0.0.1:8080}"
    : "${OC_PLACEMENTS:?set it, for example primary=./.secrets/primary.dsn}"
    : "${OC_DEFAULT_PLACEMENT:?set it, for example primary}"

    echo "Starting the control plane with ${provider}/${DEFAULT_MODEL}."
    exec go run ./cmd/controlplane
    ;;

  config)
    cat <<'CONFIG'
WHICH CONFIGURATION IS USED, AND BY WHAT

cmd/redherring — FLAGS only. It builds one provider directly and never reads the environment.
  -provider          which vendor answers (anthropic, zai)
  -model             the exact model identifier; no suffix is ever appended
  -key-file          path to the credential; required, never an environment value
  -effort            low | medium | high | xhigh | max
  -base-url          override the provider host; that host is the only one it may reach
  -transcript        where to write the recording commit CI would replay
  -deadline          wall clock for the whole round
  -request-timeout   wall clock for one call

cmd/controlplane — ENVIRONMENT only. With no model provider configured it behaves exactly as it
does today: a recorded transcript if one is named, and honest failed rounds otherwise.
  OC_MODEL_PROVIDER                 anthropic | zai. Unset means no live provider.
  OC_MODEL_NAME                     the exact model identifier
  OC_MODEL_KEY_FILE                 path to the credential
  OC_MODEL_EFFORT                   how hard to think
  OC_MODEL_BASE_URL                 override the provider host
  OC_MODEL_MAX_OUTPUT               ceiling on one answer, in tokens
  OC_MODEL_MAX_PROMPT               refuse an oversized deliberation before sending it
  OC_MODEL_CONSENTED_PROVIDERS      comma-separated. Nothing listed permits nothing.
  OC_MODEL_COST_CEILING_MICROCENTS  spending limit across rounds; unset means none

  OC_MODEL_FALLBACK_PROVIDER        the one optional fallback hop. Never inferred: a deployment
  OC_MODEL_FALLBACK_NAME            that configures none gets an honest failure rather than a
  OC_MODEL_FALLBACK_KEY_FILE        vendor nobody chose. It needs its own consent entry.
  OC_MODEL_FALLBACK_BASE_URL

  OC_MODEL_TRANSCRIPT_FILE          replay a recording instead. A live provider outranks it.

PRICED MODELS. A model with no declared rate is refused at startup, because a round costed at zero
silently disables the cost ceiling and the failure would first be noticed as a bill.
  anthropic  claude-opus-5, claude-opus-4-8, claude-sonnet-5, claude-haiku-4-5
  zai        glm-5.2, glm-5, glm-4.7, glm-4.6
CONFIG
    ;;

  *)
    echo "usage: $0 {scenario|controlplane|config}" >&2
    exit 2
    ;;
esac
