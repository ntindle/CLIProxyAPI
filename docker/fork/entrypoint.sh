#!/bin/sh
# Entrypoint of the ntindle fork image.
#
# Everything the proxy writes lives under one data directory:
#   config.yaml   configuration (created on first start, then owned by the proxy and its panel)
#   auths/        OAuth credential files
#   logs/         application and request logs
#   plugins/      plugins installed from the plugin store
#   static/       the management panel downloaded at runtime
set -eu

DATA_DIR="${CLIPROXY_DATA_DIR:-/data}"
CONFIG_FILE="${CLIPROXY_CONFIG:-${DATA_DIR}/config.yaml}"
DEFAULT_CONFIG="${CLIPROXY_DEFAULT_CONFIG:-/CLIProxyAPI/config.default.yaml}"
NEWLINE='
'

fail() {
    echo "cliproxyapi-entrypoint: $*" >&2
    exit 1
}

case "${DATA_DIR}" in
    *[!A-Za-z0-9._/-]*) fail "CLIPROXY_DATA_DIR may only contain letters, digits and ._/- characters" ;;
esac

mkdir -p "${DATA_DIR}/auths" "${DATA_DIR}/logs" "${DATA_DIR}/plugins" "${DATA_DIR}/static" \
    || fail "cannot create ${DATA_DIR}; is the data path mounted read/write?"

# Logs and panel assets follow WRITABLE_PATH; keep them beside the configuration.
export WRITABLE_PATH="${WRITABLE_PATH:-${DATA_DIR}}"

# Follow the container time zone when the image knows it.
if [ -n "${TZ:-}" ] && [ -f "/usr/share/zoneinfo/${TZ}" ]; then
    ln -snf "/usr/share/zoneinfo/${TZ}" /etc/localtime 2>/dev/null || true
    echo "${TZ}" > /etc/timezone 2>/dev/null || true
fi

if [ ! -s "${CONFIG_FILE}" ]; then
    keys="${CLIPROXY_API_KEYS:-}"
    generated=0
    if [ -z "${keys}" ]; then
        keys="sk-cpa-$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        generated=1
    fi

    # One YAML list item per comma-separated key.
    key_lines=""
    old_ifs="${IFS}"
    IFS=','
    for key in ${keys}; do
        key="$(printf '%s' "${key}" | tr -d '[:space:]')"
        [ -n "${key}" ] || continue
        case "${key}" in
            *[!A-Za-z0-9._~+/=-]*)
                fail "CLIPROXY_API_KEYS may only contain letters, digits and ._~+/=- characters"
                ;;
        esac
        key_lines="${key_lines}    - \"${key}\"${NEWLINE}"
    done
    IFS="${old_ifs}"
    [ -n "${key_lines}" ] || fail "CLIPROXY_API_KEYS is set but contains no usable key"

    tmp_config="${CONFIG_FILE}.tmp"
    (
        umask 077
        while IFS= read -r line || [ -n "${line}" ]; do
            case "${line}" in
                *__API_KEYS__*) printf '%s' "${key_lines}" ;;
                *) printf '%s\n' "${line}" ;;
            esac
        done < "${DEFAULT_CONFIG}" | sed "s|__DATA_DIR__|${DATA_DIR}|g" > "${tmp_config}"
    ) || fail "cannot write ${tmp_config}"
    [ -s "${tmp_config}" ] || fail "generated configuration is empty"
    mv "${tmp_config}" "${CONFIG_FILE}"

    echo "cliproxyapi-entrypoint: created ${CONFIG_FILE}"
    if [ "${generated}" -eq 1 ]; then
        echo "cliproxyapi-entrypoint: generated a client API key; read it from access.api-keys in ${CONFIG_FILE} or the management panel"
    fi
fi

if [ -z "${MANAGEMENT_PASSWORD:-}" ]; then
    echo "cliproxyapi-entrypoint: MANAGEMENT_PASSWORD is not set; the management panel stays disabled unless management.secret-key is set in ${CONFIG_FILE}"
fi

cd /CLIProxyAPI
exec ./CLIProxyAPI --config "${CONFIG_FILE}" "$@"
