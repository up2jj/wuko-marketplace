# Kubernetes plugin

`kube` runs a trusted Wuko workflow in a native Kubernetes Job while the calling
Wuko process remains attached. It supplies eight SDK v3 steps, a Linux PID-1
supervisor, a worker image recipe, and companion workflow packages. It requires
the Wuko **v0.18.0** SDK/CLI contract and Kubernetes **1.35+**. There is no custom controller, CRD,
Helm chart, or change to Wuko's CLI.

## Build and install locally

From the marketplace repository root:

```sh
# Build/import the native plugin and regenerate the local catalog.
wuko marketplace plugin release --version 0.1.0 \
  --description 'Supervised Kubernetes Jobs and retained execution records' ./plugin-src/kube
wuko plugin install ./plugins/kube/plugin.json

# Install the companion workflows from this local catalog.
wuko install --package kube-run --package kube-list --package kube-status \
  --package kube-watch --package kube-logs --package kube-report \
  --package kube-cancel --package kube-purge .

# Provision the namespace and worker permissions. Bind wuko-orchestrator to
# the user or automation identity that will run the plugin, using a RoleBinding.
kubectl apply -f plugin-src/kube/deploy/namespace.yaml

docker build -t registry.example.com/wuko-worker:0.1.0 plugin-src/kube
# Push to your registry before submitting a Job on a remote cluster.
docker push registry.example.com/wuko-worker:0.1.0
```

No image or remote release is published by the build helpers. For both worker
architectures use `docker buildx build --platform linux/amd64,linux/arm64 -t
YOUR_IMAGE --push plugin-src/kube`. Pin production images by digest in `image:`;
a tag selects the image version and a digest fixes its contents. The recipe pins
Wuko v0.18.0; extensions must retain `/usr/local/bin/wuko-supervisor` and its
entrypoint. The Job always overrides the image command with that supervisor.

[`examples/basic/Dockerfile`](examples/basic/Dockerfile) adds an image-local
workflow to the base image:

```sh
docker build --build-arg WORKER_IMAGE=registry.example.com/wuko-worker:0.1.0 \
  -t registry.example.com/wuko-example:1.0.0 plugin-src/kube/examples/basic
```

Use a local kind cluster with `kind load docker-image` instead of pushing.
`integration/run.sh` creates its own cluster and kubeconfig, builds test images,
runs traffic/crash tests, and deletes that cluster on exit. It needs Go 1.26,
Docker, kind, kubectl, and Wuko v0.18.0+.

Newer Wuko versions that require workflow-scoped plugin declarations also need
`plugins.kube.source` and their normal lockfile. For a local bundle, set the
source to the absolute `plugins/kube/plugin.json` path and run
`wuko plugin lock PATH_TO_WORKFLOW`. For a published bundle, use a pinned Git
reference. The integration fixture supports both CLI configurations.

## Run and inspect

```sh
wuko run kube-run --var image=registry.example.com/wuko-example:1.0.0 \
  --var workflow=/workflows/wuko.yaml --var group=staging-operations
# Prints execution_id=wk-<128 random bits>. Save that ID for later inspection.

wuko run kube-list --var label_selector='team=platform,environment=staging'
wuko run kube-status --var execution_id=wk-...
wuko run kube-watch --var execution_id=wk-...
wuko run kube-logs --var execution_id=wk-...
wuko run kube-report --var execution_id=wk-...
wuko run kube-cancel --var execution_id=wk-...
wuko run kube-purge --var execution_id=wk-...
```

All steps accept `kubeconfig`, `context`, and `namespace` (default `wuko-jobs`).
Kubeconfig uses client-go's normal loading rules, including `KUBECONFIG`, then
in-cluster authentication when no kubeconfig is available. Caller credentials
are never sent to the Job. The companion run workflow covers common options;
write a `kube.run` step for the full typed configuration, as in
[`examples/run.yaml`](examples/run.yaml).

| Step | Behavior |
| --- | --- |
| `kube.run` | Submit, renew authorization, stream logs, wait for termination, return report and declared outputs |
| `kube.list` | Retained executions; optional `label_selector` is ANDed with managed-resource selectors |
| `kube.status` | Native Pod/Job state, waiting reasons, metadata, record and termination confirmation |
| `kube.watch` | Stream changed status until confirmed termination; never renew authorization |
| `kube.logs` | Pod logs; `follow` defaults false, `tail_lines` defaults 100; companion follows |
| `kube.report` | Retrieve status and the retained final report, including unavailable states |
| `kube.cancel` | Revoke irreversibly, suspend Job, wait up to 30s; distinguish stop requested from confirmed |
| `kube.purge` | Remove records, Job and retention finalizers only after confirmed termination |

Inspection succeeds when the inspected workload failed. `kube.run` fails for an
unsuccessful workload or unavailable report. Outputs include `value` (full status,
or list), `execution_id`, `phase`, `termination_confirmed`, `report_state`,
`report` (complete schema-v2 document), and `outputs` (declared workflow outputs).
Logs and purge expose only their relevant outputs. Companion workflows return
`result`; logs stream directly. SDK streams carry logs, leaving plugin stdout
available exclusively for the JSONL protocol.

An explicit `execution_id` must be a DNS label of at most 48 characters. Reusing
it with identical configuration returns an already terminated outcome, or an
ownership conflict while active. Changed configuration conflicts. Work never
restarts under a retained ID to recover a report. Automatically generated IDs
are stable through API retries within one invocation; a new invocation gets a
new ID. Retrying whole Wuko steps needs an explicit ID for deduplication.

## Worker inputs

`image` is required. Select exactly one of:

- `workflow`: absolute path already present in the image.
- `workflow_inline`: complete YAML, mounted as an immutable input snapshot. Local
  validation checks YAML shape; Wuko in the worker validates actual execution.

Optional fields are `target`, `workdir` (default `/work`), `vars`, `resources`,
`env`, `envFrom`, `volumes`, `imagePullSecrets`, `nodeSelector`, `tolerations`,
and `serviceAccount` (default `wuko-worker`). There are no arbitrary Pod templates,
sidecars, host namespaces, hostPath mounts, or privileged workers. `volumes` is a
list of `{name, mountPath, secret}` or `{name, mountPath, configMap}` entries;
mounts are read-only and cannot overlap supervisor binaries, control files or
service-account credentials. The combined serialized input is capped at 256 KiB
and rejected before submission if oversized.

The outer Wuko workflow renders `with:` normally, including inline YAML. Escape
worker-side Go-template delimiters, e.g. `{{ "{{ .vars.message }}" }}`, when they
must reach the worker unchanged. Shell `$NAME` references need no escaping.
Image-local files are convenient for workflows with many templates or resources.

### Existing Secrets, ConfigMaps and downward API fields

```yaml
- id: remote
  type: kube.run
  with:
    image: registry.example.com/wuko-example@sha256:YOUR_DIGEST
    workflow: /workflows/wuko.yaml
    envFrom:
      - configMapRef: {name: application-config}
      - secretRef: {name: application-secrets}
      - prefix: EXTRA_
        configMapRef: {name: extra-settings, optional: true}
    env:
      - name: LOG_LEVEL
        value: info
      - name: TOKEN
        valueFrom:
          secretKeyRef: {name: credentials, key: token}
      - name: EXECUTION_ID
        valueFrom:
          fieldRef:
            fieldPath: metadata.labels['wuko.dev/execution-id']
      - name: AGENT_HOST
        valueFrom: {fieldRef: {fieldPath: status.hostIP}}
```

References resolve in the worker namespace. Later `envFrom` sources override
earlier imports; explicit `env` overrides imports. Key refs support Kubernetes'
`optional` behavior. `fieldRef.apiVersion` defaults to `v1`; supported fields are
Pod name/namespace/UID, selected labels/annotations, node name, service account,
host IP(s) and Pod IP(s). Missing required sources appear in native waiting
reasons. Source updates do not refresh an existing process environment or rerun
work. Fingerprints include reference names, order, prefixes and optional flags,
not source values or resource versions.

The plugin never reads, enumerates, copies or stores referenced Secret values.
No Secret-read RBAC is required for the caller. Kubernetes loads them into the
Pod. As with native Jobs, permission to create workloads can grant indirect
access to Secrets in that namespace; choose the namespace and caller RBAC
accordingly.

`WUKO_KUBE_` is reserved. Explicit variables and import prefixes using it are
rejected. Unprefixed imports can contain reserved keys without being inspected:
recognized supervisor variables are injected explicitly and override imports;
unknown reserved variables are ignored, and all are removed from the child Wuko
environment. Execution identity, timers and resource names come from the fixed
immutable control file. The supervisor's Kubernetes client uses a fixed
in-cluster endpoint, mounted token and explicit CA, with environment proxies
disabled. Imported `KUBECONFIG`, proxy and certificate settings cannot redirect
that client; ordinary settings remain available to the workload.

### Custom metadata

`metadata.labels` and `metadata.annotations` are string maps copied to Job,
Pod template, authorization Lease, input ConfigMap and result ConfigMap.
`wuko.dev/*` and `app.kubernetes.io/managed-by` are reserved. Invalid metadata is
rejected, never truncated. Shared group guards and deployment/RBAC resources do
not inherit execution metadata. Metadata participates in the fingerprint and is
returned by inspection. Labels help discovery; they never authorize execution or
identify the current guard holder.

## Crash safety and concurrency

Defaults are `authorization_ttl: 15s`, `renew_interval: 3s`,
`shutdown_grace: 5s`, and `max_duration: 30m`. TTL must be at least three renewal
intervals; renewal is at least 1s, TTL at most 1m, grace at most 30s and duration
at most 24h. API requests are bounded independently to 2s.

The plugin reserves a result record, creates authorization, acquires any group,
creates a suspended Job, records its UID and immutable inputs, and unsuspends it.
A worker needs an increasing heartbeat observed after startup, its expected owner
and group token, and a successful atomic Pod-UID claim before launching Wuko.
Duplicate Pods cannot claim the same execution, even after its first worker
exits. Kubernetes' Job retry count is zero.

The PID-1 supervisor checks authorization on an independent local timer. Repeated
reads of the same sequence do not refresh it. Plugin/Wuko death, stdin EOF,
cancellation or API failure stops authorization; no observer takes ownership.
After TTL expiry the worker signals Wuko, allows the configured grace, kills
remaining processes across process groups and exits its private PID namespace.
It also kills leftover descendants on normal completion. Reporting happens only
after workload termination, with a separate 2s publication budget. Logs and API
requests cannot extend the workload deadline. Remote services or external jobs
spawned by workflow code need their own cancellation mechanism.

```yaml
concurrency:
  group: staging-load-tests
  on_busy: error
```

One execution holds this group across all cooperating callers and execution IDs
in the same cluster and namespace. Atomic create/resourceVersion CAS operations
choose its holder. A busy result identifies the blocking execution. An expired
Lease or deletion request is insufficient evidence to release a group. A later
run can reconcile a revoked/expired holder only after the old Job is suspended
and every retained Pod is terminal. Unknown termination remains blocked. Late
old Pods fail their token or claim check; stale cleanup cannot release a newer
reservation. Different groups are independent. There is no waiting queue or
background cleanup controller.

Job suspension deletes active Pods. A `wuko.dev/retain` Pod finalizer keeps the
Pod record available until explicit purge can verify its terminal phase. Purge
removes only this finalizer, preserving foreign ones. It records confirmed
termination before deleting resources so an interrupted purge can resume. Do
not force-delete Pods or remove their finalizers to bypass this check: a missing
claimed Pod makes termination unknown and deliberately blocks group reuse.
Finalizers may require manual cleanup if the whole namespace is removed outside
the plugin; first establish that its workers have stopped.

The namespace and workloads are trusted, not a hostile multi-tenant sandbox.
Workers can read Leases and read/update ConfigMaps but cannot renew Leases or
manage Jobs. Workload code shares the worker UID and mounted service-account
credentials. Environment protection prevents accidental configuration override;
it is not isolation from malicious workflow code or cluster administrators.
Local PID timers also cannot guarantee a wall-clock bound while a node/kernel
is frozen. Hard external traffic fencing needs enforcement outside that node.

## Reports, progress and retention

Live progress is Wuko's plain reporter and Pod logs. Structured status comes from
native conditions and supervisor records, never parsed log percentages. A 1 MiB
plugin queue isolates slow log consumers; overflow emits an explicit log-gap
notice. Reconnects may replay logs. Logs remain subject to kubelet rotation and
Pod/node availability; this plugin is not durable log storage.

The result ConfigMap contains `record.json`, including the complete Wuko
schema-v2 final report capped at **256 KiB**:

| `report_state` | Meaning |
| --- | --- |
| `available` | Complete valid schema-v2 report stored in the result |
| `missing` | Worker finished but no report file exists |
| `invalid` | File is unreadable, malformed or not the expected schema |
| `oversized` | Report exceeds the cap; no partial outputs are retained |
| `unpublished` | Publication has not happened or the worker could not publish |

If the worker/API is unavailable, inspection cannot infer the more specific
file state. It preserves `unpublished` with native termination information.
Publication never delays workload termination, silently truncates outputs, or
reruns work. Reports contain declared outputs, so do not declare credentials as
outputs; input ConfigMaps also contain user-supplied literal `vars`/`env`.
Use Secret references for credentials.

Jobs, input/result ConfigMaps, authorization Leases and retained Pod records
remain until `kube.purge`. No TTL cleanup is installed. Completed workloads are
stopped and their groups can be released even while these records remain.
Group ConfigMaps are reused coordination records and are retained separately.

## k6 to Datadog example

[`examples/k6/run.yaml`](examples/k6/run.yaml) uses a k6 image extending the worker,
inline generic Wuko logic, `envFrom`, and `fieldRef: status.hostIP`. Build
[`examples/k6/Dockerfile`](examples/k6/Dockerfile), put `TARGET_URL` in an existing
`k6-settings` ConfigMap, and enable the existing **same-node Datadog Agent** OTLP
HTTP receiver on port 4318. Permit Pod-to-node traffic on that port. The Agent
holds the Datadog API key; the workload sends OTLP to that Agent. This example
uses plaintext on an internal network; configure TLS for networks that need it.

k6's native `--out opentelemetry` sends metric tags as OTLP attributes. The
example passes execution ID and environment explicitly; Kubernetes labels do not
automatically become metric tags. Metric time series go to Datadog; the result
ConfigMap stores Wuko's execution outcome. Stopping work takes priority over
telemetry flushing, so final samples can be lost. No Agent installation, sidecar,
k6 runtime, or Datadog-specific option exists in the plugin.

References: [Kubernetes Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/),
[environment sources](https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/),
[k6 OpenTelemetry output](https://grafana.com/docs/k6/latest/results-output/real-time/opentelemetry/),
[Datadog OTLP ingestion](https://docs.datadoghq.com/opentelemetry/interoperability/otlp_ingest_in_the_agent/).
