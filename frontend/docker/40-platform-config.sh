#!/bin/sh
# Writes /platform-config.json for platform mode (HAMSP) from the environment at
# container start, so one image serves every environment. Without
# PLATFORM_OIDC_ISSUER the file is not written and the SPA uses KANTOR's own sign-in.
set -eu

target=/usr/share/nginx/html/platform-config.json
rm -f "$target"

if [ -z "${PLATFORM_OIDC_ISSUER:-}" ]; then
  exit 0
fi

: "${PLATFORM_OIDC_CLIENT_ID:?PLATFORM_OIDC_CLIENT_ID is required with PLATFORM_OIDC_ISSUER}"
: "${PLATFORM_OIDC_AUDIENCE:?PLATFORM_OIDC_AUDIENCE is required with PLATFORM_OIDC_ISSUER}"

for value in "$PLATFORM_OIDC_ISSUER" "$PLATFORM_OIDC_CLIENT_ID" "$PLATFORM_OIDC_AUDIENCE"; do
  case "$value" in
    *[\"\\]*) echo "platform config values must not contain quotes or backslashes" >&2; exit 1 ;;
  esac
done

printf '{"issuer":"%s","clientId":"%s","audience":"%s"}\n' \
  "$PLATFORM_OIDC_ISSUER" "$PLATFORM_OIDC_CLIENT_ID" "$PLATFORM_OIDC_AUDIENCE" > "$target"
echo "platform mode: wrote $target"
