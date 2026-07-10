#!/usr/bin/env bash
# Rolls the BUSL-1.1 "Change Date" in LICENSE forward to (today + window years),
# but never backward. Run on a schedule (see .github/workflows/license-rolling-window.yml)
# so the Change Date stays a fixed distance ahead of the present while the repo
# is under active development, instead of drifting into the past.
set -euo pipefail

WINDOW_YEARS="${LICENSE_WINDOW_YEARS:-4}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LICENSE_FILE="${REPO_ROOT}/LICENSE"

current_date="$(sed -n 's/^Change Date: \([0-9-]*\).*/\1/p' "$LICENSE_FILE")"
if [[ -z "$current_date" ]]; then
	echo "error: could not find 'Change Date: YYYY-MM-DD' line in $LICENSE_FILE" >&2
	exit 1
fi

new_date="$(date -u -d "+${WINDOW_YEARS} years" +%Y-%m-%d)"

if [[ "$new_date" > "$current_date" ]]; then
	sed -i "s/^Change Date: .*/Change Date: ${new_date}/" "$LICENSE_FILE"
	echo "Bumped Change Date: ${current_date} -> ${new_date}"
	echo "changed=true" >>"${GITHUB_OUTPUT:-/dev/stdout}"
else
	echo "Change Date ${current_date} already >= today + ${WINDOW_YEARS}y (${new_date}); no update needed."
	echo "changed=false" >>"${GITHUB_OUTPUT:-/dev/stdout}"
fi
