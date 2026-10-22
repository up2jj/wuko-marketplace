ARG WORKER_IMAGE=wuko-worker:dev
FROM ${WORKER_IMAGE}
COPY --chown=10001:10001 wuko.yaml /workflows/wuko.yaml
# Preserve the inherited supervisor entrypoint.
