#!/usr/bin/env bash

set -euo pipefail

CLEANUPS=()
trap 'set +e; for I in ${!CLEANUPS[@]}; do eval "${CLEANUPS[-($I+1)]}"; done' EXIT

COVER_PROFILE_FILE=$(mktemp)
CLEANUPS+=("rm ${COVER_PROFILE_FILE@Q}")
go test -coverprofile="${COVER_PROFILE_FILE}" ./... >/dev/null

BADGE_CONTENT=$(go tool cover -func "${COVER_PROFILE_FILE}" | awk 'match($0, "^total:\\s+\\(statements\\)\\s+([0-9]+(\\.[0-9]+)?)%$", groups) {
  cover_percent = groups[1]
  if (cover_percent >= 70.0) {
    color = "brightgreen"
  } else if (cover_percent >= 30.0) {
    color = "yellow"
  } else {
    color = "red"
  }
  printf "Coverage-%s%%25-%s", cover_percent, color
}')

DIR=$(dirname "$(realpath "${0}")")
curl --silent --output "${DIR}/coverage.svg" "https://img.shields.io/badge/${BADGE_CONTENT}"
