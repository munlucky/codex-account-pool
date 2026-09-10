#!/bin/bash
set -euo pipefail

repo_root="$(cd "$(dirname "$0")" && pwd)"
state_root="${HOME}/Library/Application Support/GPTCodexRouter"
registry_path="${state_root}/registry.json"
codex_config_path="${HOME}/.codex/config.toml"
compose_env_path="${repo_root}/.env"
profile_pattern='^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'
skip_codex_config=0
no_start=0
profiles_arg=""
known_profiles=()
active_profile=""

usage() {
  cat <<'USAGE'
Usage: ./setup.sh [options]

Options:
  --profiles name1,name2   Sign in or refresh explicit profile names.
  --skip-codex-config      Do not edit ~/.codex/config.toml.
  --no-start               Register logins only; do not start Docker.
  -h, --help               Show this help.
USAGE
}

fail() {
  printf 'Error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "Required command '$1' was not found in PATH."
}

contains_profile() {
  local needle="$1"
  local item
  for item in "${known_profiles[@]:-}"; do
    [ "$item" = "$needle" ] && return 0
  done
  return 1
}

add_profile() {
  local profile_id="$1"
  if ! contains_profile "$profile_id"; then
    known_profiles+=("$profile_id")
  fi
}

next_profile_name() {
  local index=1
  while contains_profile "account-${index}"; do
    index=$((index + 1))
  done
  printf 'account-%s\n' "$index"
}

read_existing_registry() {
  [ -f "$registry_path" ] || return 0

  local profile_id
  while IFS= read -r profile_id; do
    if [[ "$profile_id" =~ $profile_pattern ]]; then
      add_profile "$profile_id"
    fi
  done < <(sed -nE 's/^[[:space:]]*"id"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$registry_path")

  active_profile="$(sed -nE 's/^[[:space:]]*"codex"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/p' "$registry_path" | head -n 1)"
  if [ -n "$active_profile" ] && ! [[ "$active_profile" =~ $profile_pattern ]]; then
    active_profile=""
  fi
}

write_registry() {
  mkdir -p "$state_root"
  local tmp="${registry_path}.tmp.$$"
  local index=0
  local count="${#known_profiles[@]}"
  {
    printf '{\n  "version": 1,\n  "profiles": [\n'
    local profile_id
    for profile_id in "${known_profiles[@]}"; do
      index=$((index + 1))
      printf '    {\n      "id": "%s",\n      "provider": "codex",\n      "isolation": "codex-home"\n    }' "$profile_id"
      [ "$index" -lt "$count" ] && printf ','
      printf '\n'
    done
    printf '  ],\n  "active": {\n    "codex": "%s"\n  }\n}\n' "$active_profile"
  } > "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$registry_path"
}

write_compose_env() {
  local escaped_state_root
  escaped_state_root="${state_root//\'/\\\'}"
  printf "GPT_CODEX_ROUTER_STATE_ROOT='%s'\n" "$escaped_state_root" > "$compose_env_path"
}

invoke_codex_login() {
  local profile_id="$1"
  [[ "$profile_id" =~ $profile_pattern ]] || fail "Invalid profile name '$profile_id'. Use letters, numbers, dot, underscore, or dash (max 64 chars)."

  local codex_home="${state_root}/profiles/codex/${profile_id}"
  mkdir -p "$codex_home"

  printf '\n=== ChatGPT login: %s ===\n' "$profile_id"
  printf 'Complete the official Codex login flow in the browser, then return here.\n'

  (
    export CODEX_HOME="$codex_home"
    unset OPENAI_API_KEY CODEX_API_KEY CODEX_ACCESS_TOKEN || true
    codex -c 'cli_auth_credentials_store="file"' login
  ) || fail "Codex login failed for profile '$profile_id'."

  [ -f "${codex_home}/auth.json" ] || fail "Codex reported success but ${codex_home}/auth.json was not created."
}

set_codex_desktop_config() {
  local config_dir
  config_dir="$(dirname "$codex_config_path")"
  mkdir -p "$config_dir"

  local backup="${codex_config_path}.gpt-codex-router.bak"
  if [ -f "$codex_config_path" ] && [ ! -f "$backup" ]; then
    cp "$codex_config_path" "$backup"
    printf 'Backed up original Codex config to %s\n' "$backup"
  fi

  local preserved="$(mktemp "${TMPDIR:-/tmp}/gpt-codex-router-config.XXXXXX")"
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

  local tmp="${codex_config_path}.tmp.$$"
  {
    printf 'chatgpt_base_url = "http://127.0.0.1:8317/backend-api"\n'
    printf 'openai_base_url = "http://127.0.0.1:8317/backend-api/codex"\n\n'
    cat "$preserved"
  } > "$tmp"
  mv "$tmp" "$codex_config_path"
  rm -f "$preserved"
  printf 'Configured Codex Desktop routing in %s\n' "$codex_config_path"
}

stop_existing_router() {
  (cd "$repo_root" && docker compose stop gpt-codex-router >/dev/null 2>&1) || true
}

start_router() {
  (cd "$repo_root" && docker compose up -d --build) || fail 'docker compose up failed.'

  local attempt
  for attempt in $(seq 1 20); do
    if curl --fail --silent --show-error --max-time 2 http://127.0.0.1:8317/healthz 2>/dev/null | grep -qx 'ok'; then
      return 0
    fi
    sleep 0.5
  done
  fail 'Container started, but the local health check did not become ready. Run: docker compose logs --tail=100 gpt-codex-router'
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --profiles)
      [ "$#" -ge 2 ] || fail '--profiles requires a comma-separated value.'
      profiles_arg="$2"
      shift 2
      ;;
    --skip-codex-config)
      skip_codex_config=1
      shift
      ;;
    --no-start)
      no_start=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "Unknown option: $1"
      ;;
  esac
done

[ "$(uname -s)" = 'Darwin' ] || fail 'setup.sh is intended for macOS. Use setup.cmd/setup.ps1 on Windows.'
require_command codex
require_command docker
require_command curl

docker compose version >/dev/null 2>&1 || fail 'Docker Compose v2 is required. Install/start Docker Desktop and rerun setup.'
docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 || fail 'Docker Desktop is not running or its engine is unavailable.'

mkdir -p "$state_root"
chmod 700 "$state_root"
write_compose_env
stop_existing_router
read_existing_registry

if [ -n "$profiles_arg" ]; then
  old_ifs="$IFS"
  IFS=','
  set -- $profiles_arg
  IFS="$old_ifs"
  for profile_id in "$@"; do
    [ -n "$profile_id" ] || fail 'Profile names cannot be empty.'
    invoke_codex_login "$profile_id"
    add_profile "$profile_id"
    if [ -z "$active_profile" ]; then
      active_profile="$profile_id"
    fi
    write_registry
  done
else
  printf 'GPT Codex Router setup\n'
  printf 'Sign in to each ChatGPT account you want to route. Profile names are assigned automatically.\n'
  while :; do
    profile_id="$(next_profile_name)"
    printf 'Using local profile name: %s\n' "$profile_id"
    invoke_codex_login "$profile_id"
    add_profile "$profile_id"
    if [ -z "$active_profile" ]; then
      active_profile="$profile_id"
    fi
    write_registry
    printf 'Sign in to another ChatGPT account? [y/N] '
    IFS= read -r another
    case "$another" in
      y|Y|yes|YES|Yes) ;;
      *) break ;;
    esac
  done
fi

[ "${#known_profiles[@]}" -gt 0 ] || fail 'At least one ChatGPT/Codex profile is required.'
if [ -z "$active_profile" ] || ! contains_profile "$active_profile"; then
  active_profile="${known_profiles[0]}"
fi
write_registry
printf 'Saved %s profile(s). Active profile: %s\n' "${#known_profiles[@]}" "$active_profile"

if [ "$no_start" -eq 0 ]; then
  start_router
  if [ "$skip_codex_config" -eq 0 ]; then
    set_codex_desktop_config
  fi
  printf '\nGPT Codex Router is running at http://127.0.0.1:8317\n'
  if [ "$skip_codex_config" -eq 0 ]; then
    printf 'Restart Codex Desktop completely before using it.\n'
  fi
  printf 'Logs: docker compose logs -f --tail=100 gpt-codex-router\n'
else
  printf 'Login setup complete. Start the router later with: docker compose up -d --build\n'
  if [ "$skip_codex_config" -eq 0 ]; then
    printf 'Codex Desktop config was not changed because --no-start was used.\n'
  fi
fi
