#!/usr/bin/env bash
# lint-no-typography-in-strings.sh - fail if a Go string literal carries an em
# dash, en dash, curly quote, unicode ellipsis or unicode bullet.
#
# A string literal ships: it is the error a client reads, the log line an
# operator greps, the message a runbook quotes. A document that tells someone
# what to grep for has to quote the string exactly, and typography that looks
# alike but differs breaks that grep silently.
#
# Comments are not checked here. They are prose and do not ship in a response.
#
# Suppress a genuine false positive with //lyeve:allow-typography plus a reason,
# on the offending line. The case that earns it is a string that must match
# something outside our control, such as a third party's exact response text.

set -uo pipefail
LC_ALL=C.UTF-8
export LC_ALL

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

findings=0
while IFS= read -r f; do
  case "$f" in */vendor/*|*/node_modules/*|*/.git/*) continue ;; esac
  # Strip the comment tail, then look for the characters inside a "..." literal.
  awk -v file="$f" '
    /\/\/lyeve:allow-typography/ { next }
    {
      line = $0
      sub(/\/\/.*$/, "", line)
      n = split(line, parts, /"/)
      for (i = 2; i <= n; i += 2) {
        # An alternation of exact three-byte sequences. A bracket expression
        # here would be a class of the individual bytes, and \342\200 is the
        # prefix of most of the multibyte plane, so it would match Japanese
        # and emoji.
        if (parts[i] ~ /\342\200\224|\342\200\223|\342\200\230|\342\200\231|\342\200\234|\342\200\235|\342\200\246|\342\200\242/) {
          printf "%s:%d: %s\n", file, FNR, substr($0, 1, 130)
          break
        }
      }
    }
  ' "$f"
done < <(find "$ROOT" -name '*.go' -not -path '*/vendor/*' -not -path '*/node_modules/*' 2>/dev/null) > "$tmp" 2>/dev/null

# grep -c prints 0 and exits 1 on an empty file, so `|| echo 0` appends a second
# zero and the test below sees "0\n0".
findings=$(grep -c . "$tmp" 2>/dev/null)
findings=${findings:-0}
if [ "$findings" -gt 0 ]; then
  echo "lint-no-typography-in-strings: $findings string literal(s) carry forbidden typography"
  head -40 "$tmp"
  [ "$findings" -gt 40 ] && echo "  ... and $((findings - 40)) more"
  echo
  echo "Em/en dashes, curly quotes, unicode ellipses and bullets are refused in"
  echo "shipped text. A string literal ships. Rewrite it, or mark a genuine"
  echo "false positive with //lyeve:allow-typography <reason>."
  exit 1
fi
echo "lint-no-typography-in-strings: clean"
