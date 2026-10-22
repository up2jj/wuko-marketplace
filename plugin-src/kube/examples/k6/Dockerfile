ARG WORKER_IMAGE=wuko-worker:dev
FROM grafana/k6:2.3.0 AS k6
FROM ${WORKER_IMAGE}
COPY --from=k6 /usr/bin/k6 /usr/local/bin/k6
COPY --chown=10001:10001 test.js /tests/test.js
