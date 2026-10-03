#!/bin/bash
# Container health check: the proxy answers GET /healthz without authentication.
# CLIPROXY_HEALTH_PORT overrides the port; CLIPROXY_HEALTHCHECK=off disables the check
# (for example when server.tls is enabled, which this plain-HTTP probe cannot speak).
set -u

case "${CLIPROXY_HEALTHCHECK:-on}" in
    off | false | 0 | disabled) exit 0 ;;
esac

port="${CLIPROXY_HEALTH_PORT:-8317}"

exec 3<>"/dev/tcp/127.0.0.1/${port}" || exit 1
printf 'GET /healthz HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' >&3
IFS= read -r -t 4 status <&3 || exit 1
case "${status}" in
    *" 200"*) exit 0 ;;
    *) exit 1 ;;
esac
