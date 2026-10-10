#!/usr/bin/env bash
# Refuses commits and pushes that contain private data: the bridge's store, databases,
# tokens, keys, or (on a machine with a paired bridge) any real phone number or group ID.
#
#   check-private.sh staged          files about to be committed        (pre-commit hook)
#   check-private.sh commits A B...  everything added by these commits  (pre-push hook)
#   check-private.sh all             every tracked file                 (CI)
#   check-private.sh history         everything added by any commit on any branch
#
# A line that is flagged wrongly can be allowed by ending it with: private-check: allow

set -u
export LC_ALL=C
cd "$(git rev-parse --show-toplevel)" || exit 2

# Paths that must never be tracked, whatever they contain
FORBIDDEN_PATHS='(^|/)store/|\.db(-wal|-shm|-journal)?$|\.sqlite3?$|(^|/)api_token$|(^|/)\.env(\.|$)|(^|/)claude_desktop_config\.json$|\.(pem|p12|pfx|key|keystore)$|(^|/)id_(rsa|ed25519|ecdsa)|^whatsapp-bridge/bin/|\.log$|(^|/)\.DS_Store$'

# Text that must never appear in a tracked file
FORBIDDEN_TEXT='-----BEGIN [A-Z ]*PRIVATE KEY-----|(api[_-]?token|api[_-]?key|secret|passw(or)?d|authorization)["'"'"']?[[:space:]]*[:=][[:space:]]*["'"'"']?(Bearer )?[A-Za-z0-9_+/=-]{24,}|gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|sk-ant-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}'

store="${WHATSAPP_MCP_STORE_DIR:-whatsapp-bridge/store}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0

# On a machine with a paired bridge, also look for this account's real data
live_token=""
if [[ -s "$store/api_token" ]]; then
  live_token="$(tr -d '[:space:]' < "$store/api_token")"
fi
identifiers="$work/identifiers"
: > "$identifiers"
if [[ -f "$store/messages.db" ]] && command -v sqlite3 >/dev/null; then
  sqlite3 -readonly "file:$store/messages.db?mode=ro" \
    "SELECT substr(jid, 1, instr(jid, '@') - 1) FROM chats
     UNION SELECT CASE WHEN instr(sender, '@') > 0 THEN substr(sender, 1, instr(sender, '@') - 1) ELSE sender END FROM messages" \
    2>/dev/null | grep -E '^[0-9-]{8,}$' | sort -u > "$identifiers"
fi

report() { # label, path, reason
  echo "  BLOCKED  $2  ($1): $3" >&2
  failures=$((failures + 1))
}

check() { # label, path, git object holding the content
  local label="$1" path="$2" object="$3" blob="$work/blob" hits

  if [[ "$path" =~ $FORBIDDEN_PATHS ]]; then
    report "$label" "$path" "this kind of file must not be in the repository"
    return
  fi

  git cat-file blob "$object" > "$blob" 2>/dev/null || return 0

  if [[ "$(head -c 15 "$blob" | tr -d '\0')" == "SQLite format 3" ]]; then
    report "$label" "$path" "it is a SQLite database"
    return
  fi
  if [[ -n "$live_token" ]] && grep -aqF -- "$live_token" "$blob"; then
    report "$label" "$path" "it contains this machine's bridge API token"
  fi

  # The remaining rules read text; skip binary files such as images
  grep -Iq . "$blob" 2>/dev/null || return 0

  hits="$(grep -nEi -- "$FORBIDDEN_TEXT" "$blob" | grep -v 'private-check: allow' | cut -c1-60 | head -3)"
  if [[ -n "$hits" ]]; then
    report "$label" "$path" "it looks like it contains a key, token or password, at line $(echo "$hits" | cut -d: -f1 | tr '\n' ' ')"
  fi
  if [[ -s "$identifiers" ]]; then
    hits="$(grep -nwFf "$identifiers" "$blob" | grep -v 'private-check: allow' | cut -d: -f1 | head -5 | tr '\n' ' ')"
    if [[ -n "$hits" ]]; then
      report "$label" "$path" "it contains a phone number or group ID from your WhatsApp account, at line $hits"
    fi
  fi
}

check_commit() {
  local commit="$1" path
  while IFS= read -r -d '' path; do
    check "commit ${commit:0:7}" "$path" "$commit:$path"
  done < <(git diff-tree --root --no-commit-id -r -m -z --name-only --diff-filter=ACMR "$commit")
}

mode="${1:-staged}"
shift || true
case "$mode" in
  staged)
    while IFS= read -r -d '' path; do
      check "staged" "$path" ":$path"
    done < <(git diff --cached -z --name-only --diff-filter=ACMR)
    ;;
  commits)
    for commit in "$@"; do check_commit "$commit"; done
    ;;
  all)
    while IFS= read -r -d '' path; do
      check "tracked" "$path" "HEAD:$path"
    done < <(git ls-files -z)
    ;;
  history)
    while IFS= read -r commit; do check_commit "$commit"; done < <(git rev-list --all)
    ;;
  *)
    echo "usage: check-private.sh staged | commits <sha>... | all | history" >&2
    exit 2
    ;;
esac

if (( failures > 0 )); then
  echo >&2
  echo "Private data check failed: $failures problem(s). Nothing was committed or pushed." >&2
  echo "Remove the file or text above and try again. See scripts/check-private.sh for the rules." >&2
  exit 1
fi
exit 0
