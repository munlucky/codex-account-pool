#!/bin/bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")" && pwd)"
default_state_root="${HOME}/Library/Application Support/GPTCodexRouter"
compose_env_path="${repo_root}/.env"
codex_config_path="${HOME}/.codex/config.toml"
skip_codex_config=0
no_start=0

usage() {
  cat <<'USAGE'
Usage: ./setup.sh [options]

Options:
  --skip-codex-config      Do not edit ~/.codex/config.toml.
  --no-start               Prepare local state only; do not build/start Docker.
  -h, --help               Show this help.
USAGE
}

fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
require_command() { command -v "$1" >/dev/null 2>&1 || fail "Required command '$1' was not found in PATH."; }

resolve_state_root() {
  if [ ! -f "$compose_env_path" ]; then
    printf '%s\n' "$default_state_root"
    return
  fi
  local raw
  raw="$(sed -nE 's/^[[:space:]]*GPT_CODEX_ROUTER_STATE_ROOT[[:space:]]*=[[:space:]]*(.*)[[:space:]]*$/\1/p' "$compose_env_path" | tail -n 1)"
  if [ -z "$raw" ]; then
    printf '%s\n' "$default_state_root"
    return
  fi
  case "$raw" in
    \'*\') raw="${raw#\'}"; raw="${raw%\'}" ;;
    \"*\") raw="${raw#\"}"; raw="${raw%\"}" ;;
  esac
  raw="${raw//\\\'/\'}"
  [ -n "$raw" ] || fail "GPT_CODEX_ROUTER_STATE_ROOT in $compose_env_path is empty."
  case "$raw" in
    /*) printf '%s\n' "$raw" ;;
    *) printf '%s/%s\n' "$repo_root" "$raw" ;;
  esac
}

state_root="$(resolve_state_root)"
client_key_path="${state_root}/client-key"

ensure_client_api_key() {
  mkdir -p "$state_root"
  chmod 700 "$state_root"
  if [ -f "$client_key_path" ]; then
    local existing
    existing="$(tr -d '\r\n' < "$client_key_path")"
    case "$existing" in
      gcr_????????????????????????????*) return 0 ;;
      *) fail "Existing local API key at $client_key_path is invalid. Remove it manually only if you intend to rotate the key." ;;
    esac
  fi
  local key
  key="gcr_$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s\n' "$key" > "$client_key_path"
  chmod 600 "$client_key_path"
}

write_compose_env() {
  local escaped tmp
  escaped="${state_root//\'/\\\'}"
  tmp="${compose_env_path}.tmp.$$"
  {
    printf "GPT_CODEX_ROUTER_STATE_ROOT='%s'\n" "$escaped"
    if [ -f "$compose_env_path" ]; then
      grep -Ev '^[[:space:]]*GPT_CODEX_ROUTER_STATE_ROOT[[:space:]]*=' "$compose_env_path" || true
    fi
  } > "$tmp"
  mv "$tmp" "$compose_env_path"
}

set_codex_desktop_config() {
  mkdir -p "$(dirname "$codex_config_path")"
  local backup="${codex_config_path}.gpt-codex-router.bak"
  if [ -f "$codex_config_path" ] && [ ! -f "$backup" ]; then
    cp "$codex_config_path" "$backup"
    printf 'Backed up original Codex config to %s\n' "$backup"
  fi
  local preserved tmp
  preserved="$(mktemp "${TMPDIR:-/tmp}/gpt-codex-router-config.XXXXXX")"
  if [ -f "$codex_config_path" ]; then
    awk '
      BEGIN { in_root = 1 }
      /^[[:space:]]*\[/ { in_root = 0 }
      in_root && /^[[:space:]]*(chatgpt_base_url|openai_base_url)[[:space:]]*=/ { next }
      { print }
    ' "$codex_config_path" > "$preserved"
  else
    : > "$preserved"
  fi
  tmp="${codex_config_path}.tmp.$$"
  {
    printf 'chatgpt_base_url = "http://127.0.0.1:8317/backend-api"\n'
    printf 'openai_base_url = "http://127.0.0.1:8317/backend-api/codex"\n\n'
    cat "$preserved"
  } > "$tmp"
  mv "$tmp" "$codex_config_path"
  rm -f "$preserved"
  printf 'Configured Codex Desktop routing in %s\n' "$codex_config_path"
}

stop_legacy_host_worker_at() {
  local root="$1" pid_path pid command
  [ -n "$root" ] || return 0
  pid_path="${root}/host-worker.pid"
  [ -f "$pid_path" ] || return 0
  pid="$(tr -dc '0-9' < "$pid_path")"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    command="$(ps -p "$pid" -o command= 2>/dev/null || true)"
    case "$command" in
      *"${root}/host-tools/"*' worker'*)
        kill "$pid" 2>/dev/null || true
        printf 'Stopped obsolete host worker PID %s\n' "$pid"
        ;;
    esac
  fi
  rm -f "$pid_path"
}

start_router() {
  (cd "$repo_root" && docker compose up -d --build) || fail 'docker compose up failed.'
  local attempt
  for attempt in $(seq 1 30); do
    if curl --fail --silent --show-error --max-time 2 http://127.0.0.1:8317/healthz 2>/dev/null | grep -qx 'ok'; then
      return 0
    fi
    sleep 0.5
  done
  fail 'Container started, but the local health check did not become ready. Run: docker compose logs --tail=100 gpt-codex-router'
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --skip-codex-config) skip_codex_config=1; shift ;;
    --no-start) no_start=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "Unknown option: $1" ;;
  esac
done

[ "$(uname -s)" = 'Darwin' ] || fail 'setup.sh is intended for macOS. Use setup.cmd/setup.ps1 on Windows.'
require_command docker
require_command curl

docker compose version >/dev/null 2>&1 || fail 'Docker Compose v2 is required. Install/start Docker Desktop and rerun setup.'
docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 || fail 'Docker Desktop is not running or its engine is unavailable.'

mkdir -p "$state_root"
chmod 700 "$state_root"
ensure_client_api_key
write_compose_env

# Migration cleanup only. New versions do not install or require a host worker.
stop_legacy_host_worker_at "$state_root"
if [ "$default_state_root" != "$state_root" ]; then
  stop_legacy_host_worker_at "$default_state_root"
fi

if [ "$no_start" -eq 1 ]; then
  printf 'Prepared router state at %s\n' "$state_root"
  printf 'Start later with: docker compose up -d --build\n'
  exit 0
fi

start_router
if [ "$skip_codex_config" -eq 0 ]; then
  set_codex_desktop_config
fi

printf '\nGPT Codex Router is running at http://127.0.0.1:8317\n'
printf 'State root: %s\n' "$state_root"
printf 'Account management UI: http://127.0.0.1:8317/admin\n'
printf 'Administrator key: docker compose exec -T gpt-codex-router gpt-codex-router admin-key\n'
printf 'Add or re-authenticate Codex accounts from /admin using Docker-managed device-code login.\n'
printf 'OpenAI-compatible base URL: http://127.0.0.1:8317/v1\n'
if [ "$skip_codex_config" -eq 0 ]; then
  printf 'Restart Codex Desktop completely before using it.\n'
fi
printf 'Logs: docker compose logs -f --tail=100 gpt-codex-router\n'
