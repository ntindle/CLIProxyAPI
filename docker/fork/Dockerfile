# syntax=docker/dockerfile:1
#
# Runtime layer of the ntindle fork image. It is built on top of the image produced by
# the repository's own Dockerfile (docker/fork/docker-bake.hcl wires the two together),
# so the upstream build recipe stays untouched and merges from upstream do not conflict.
#
# It adds a single /data volume (config, credentials, logs, plugins, panel assets), a
# first-run configuration, and a health check.

ARG BASE_IMAGE=cliproxyapi-upstream
FROM ${BASE_IMAGE}

COPY docker/fork/entrypoint.sh /usr/local/bin/cliproxyapi-entrypoint
COPY docker/fork/healthcheck.sh /usr/local/bin/cliproxyapi-healthcheck
COPY docker/fork/config.default.yaml /CLIProxyAPI/config.default.yaml

RUN chmod 0755 /usr/local/bin/cliproxyapi-entrypoint /usr/local/bin/cliproxyapi-healthcheck \
    && mkdir -p /data

ENV TZ=UTC \
    CLIPROXY_DATA_DIR=/data

VOLUME ["/data"]

EXPOSE 8317

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["cliproxyapi-healthcheck"]

ENTRYPOINT ["cliproxyapi-entrypoint"]
CMD []
