# Wuko workflow marketplace

A version-1 archive marketplace of runnable [Wuko](https://github.com/up2jj/wuko) workflows and
executable plugins. Each package demonstrates one capability of the engine, and every example
is self-contained: POSIX shell builtins and Wuko's own steps only, with no network calls,
containers, or coding agents. `vault-secrets` is the one exception, and a guarded one: it reaches
for the Bitwarden CLI when the machine has one and skips the lookup when it does not.

## Install

The current catalog is tested against [Wuko v0.13.0](https://github.com/up2jj/wuko/releases/tag/v0.13.0).
The `Since` column below records the first Wuko release that supports each example.

```sh
# Interactive picker over every package
wuko install https://github.com/up2jj/wuko-marketplace

# Non-interactive, repeatable --package
wuko install --package race-a-monitor --package lua-typed-args \
  https://github.com/up2jj/wuko-marketplace

# Install for the current user rather than the current repository
wuko install --global --package probe-exit-codes https://github.com/up2jj/wuko-marketplace
```

Remove one with `wuko uninstall NAME`.

## Packages

| Package | Demonstrates | Since |
| --- | --- | --- |
| [`hello-wuko`](.wuko/workflows/hello-wuko/wuko.yaml) | Variables and step outputs — the smallest useful workflow | — |
| [`recover-and-rollback`](.wuko/workflows/recover-and-rollback/wuko.yaml) | The `try`/`catch` control, the structured `error` root, and `recovered` | v0.11.0 |
| [`race-a-monitor`](.wuko/workflows/race-a-monitor/wuko.yaml) | The `cancel_on` control, its result contract, and workflow-level `outputs:` | v0.11.0 |
| [`probe-exit-codes`](.wuko/workflows/probe-exit-codes/wuko.yaml) | `shell.allowed_exit_codes` plus `stdout`/`stderr` capture policies | v0.9.0 |
| [`scripted-pty`](.wuko/workflows/scripted-pty/wuko.yaml) | `shell.interactions` (static and `expr`), `sensitive` sends, and `terminal` styling | v0.10.0 / v0.11.0 |
| [`choice-and-table`](.wuko/workflows/choice-and-table/wuko.yaml) | `tui_table`, computed `tui_choice` `*_expr` properties, and `auto_select_single` | v0.9.0 / v0.11.0 |
| [`cue-eval`](.wuko/workflows/cue-eval/wuko.yaml) | CUE constraints, defaults, comprehensions, policy validation, and typed step outputs | v0.13.0 + plugin |
| [`local-notifier-demo`](.wuko/workflows/local-notifier-demo/wuko.yaml) | Portable terminal and desktop notifications with automatic fallback | v0.13.0 + plugin |
| [`lua-typed-args`](.wuko/workflows/lua-typed-args/wuko.yaml) | `lua` argument expressions and the `wuko.*` runtime snapshot roots | v0.11.0 |
| [`multiplexer-status`](.wuko/workflows/multiplexer-status/wuko.yaml) | The `multiplexer` step for tmux, cmux, and Herdr, including tab scope and title restore | v0.11.0 |
| [`concurrent-dag`](.wuko/workflows/concurrent-dag/wuko.yaml) | Sibling `needs` edges inside `concurrent`, ancestor state, and descendant skipping | v0.12.0 |
| [`scoped-environments`](.wuko/workflows/scoped-environments/wuko.yaml) | `env:` blocks: nesting, restoration on exit, and what `defer` and `finally` see | v0.12.0 |
| [`structured-edit`](.wuko/workflows/structured-edit/wuko.yaml) | The `edit` step: JSONPath selection over JSON, YAML, and TOML with comments preserved | v0.12.0 |
| [`durable-state`](.wuko/workflows/durable-state/wuko.yaml) | `key_value` `expr`, atomic `update`, `variable`, `default`, `prefix`, and `clear` | v0.12.0 |
| [`run-once`](.wuko/workflows/run-once/wuko.yaml) | `once` blocks: keyed idempotency, replayed results, and `on_busy: wait` | v0.12.0 |
| [`recordable-time`](.wuko/workflows/recordable-time/wuko.yaml) | The `time` step, `workflow.timezone`, and the pure `parseTime`/`addTime`/`formatTime` helpers | v0.12.0 |
| [`vault-secrets`](.wuko/workflows/vault-secrets/wuko.yaml) | `secret()` in templates, expressions, and conditions, the per-occurrence cache, and the `secrets.ensure_auth` preflight | v0.13.0 |
| [`attempt-control`](.wuko/workflows/attempt-control/wuko.yaml) | `attempt`: one control for timeout, retry, and polling — isolated passes, `when` vs `until`, and at-least-once effects | v0.13.0 |
| [`observe-and-react`](.wuko/workflows/observe-and-react/wuko.yaml) | `observe`: background bodies driven by filesystem and shell sources, with `ignore`, `debounce`, `on_change`, and `on_error` | v0.13.0 |
| [`managed-process`](.wuko/workflows/managed-process/wuko.yaml) | `process` services with log and exec readiness, plus `rpc: jsonl` workers called through `process_call` and a pool | v0.13.0 |
| [`git-history`](.wuko/workflows/git-history/wuko.yaml) | `git_revision`, `git_merge_base`, `git_log`, `git_diff`, and `git_diff_check` as structured data | v0.13.0 |
| [`commit-policy`](.wuko/workflows/commit-policy/wuko.yaml) | `git_conventional_commit` create and validate, `git_commit` with trailers and identities, and the commit-message helpers | v0.13.0 |
| [`expression-toolbox`](.wuko/workflows/expression-toolbox/wuko.yaml) | Text, parsing, URI, encoding, hashing, number, and secure-generator helpers across templates, Expr, and Lua | v0.13.0 |

## Running an example without installing

```sh
wuko run --file .wuko/workflows/<name>/wuko.yaml
wuko run --file .wuko/workflows/<name>/wuko.yaml --dry-run   # render and validate only
wuko tree --file .wuko/workflows/<name>/wuko.yaml            # inspect structure
```

Several packages take variables that switch which branch executes:

```sh
# Watch try/catch recover, then succeed outright
wuko run --file .wuko/workflows/recover-and-rollback/wuko.yaml
wuko run --file .wuko/workflows/recover-and-rollback/wuko.yaml --var fail_deploy=false

# Let the body beat the monitor instead of losing to it
wuko run --file .wuko/workflows/race-a-monitor/wuko.yaml --var work_seconds=1

# grep matches, so the probe exits 0 instead of 1
wuko run --file .wuko/workflows/probe-exit-codes/wuko.yaml --var needle=alpha

# Skip a prerequisite, then fail one and watch only its descendants skip
wuko run --file .wuko/workflows/concurrent-dag/wuko.yaml --var run_integration=false
wuko run --file .wuko/workflows/concurrent-dag/wuko.yaml --var break_unit=true

# Change the overlaid GOOS for the whole scoped block
wuko run --file .wuko/workflows/scoped-environments/wuko.yaml --var target=darwin

# Scale every matched node by 3 instead of 1
wuko run --file .wuko/workflows/structured-edit/wuko.yaml --var scale=3

# Exhaust the retry budget, then widen it from the command line
wuko run --file .wuko/workflows/attempt-control/wuko.yaml --var failures=9
wuko run --file .wuko/workflows/attempt-control/wuko.yaml --var failures=6 --var budget=8

# Give each observer longer to react before the next edit lands
wuko run --file .wuko/workflows/observe-and-react/wuko.yaml --var edit_pause=800ms

# Grow the RPC worker pool
wuko run --file .wuko/workflows/managed-process/wuko.yaml --var workers=4

# Send commit-policy a message its own policy rejects
wuko run --file .wuko/workflows/commit-policy/wuko.yaml --var proposed_message='updated some stuff'
```

`git-history`, `commit-policy`, and `managed-process` build everything they read — a throwaway Git
repository, a service, an RPC worker — inside a managed temp directory, so they never touch your
own history or leave a process behind. `observe-and-react` ends with an explicit `return`, which
cancels and joins its observers instead of watching until you interrupt it.

Three packages keep state between runs, so run them twice:

```sh
# The counter climbs; --var reset=true clears the store again
wuko run --file .wuko/workflows/durable-state/wuko.yaml
wuko run --file .wuko/workflows/durable-state/wuko.yaml
wuko run --file .wuko/workflows/durable-state/wuko.yaml --var reset=true

# The second run does no work but republishes the recorded result;
# a new key makes it real work again, and a fresh race key reruns the race
wuko run --file .wuko/workflows/run-once/wuko.yaml
wuko run --file .wuko/workflows/run-once/wuko.yaml
wuko run --file .wuko/workflows/run-once/wuko.yaml --var version=3 --var race_key=take-2

# Pin the captured clock and the whole run becomes reproducible
wuko run --file .wuko/workflows/recordable-time/wuko.yaml
wuko run --file .wuko/workflows/recordable-time/wuko.yaml --var stamp=2026-08-29T09:15:00Z
```

`durable-state` and `run-once` write to `.wuko/values/` beside the workflow. `structured-edit`
edits only files it creates in its own temp directory.

`choice-and-table` prompts. Supply both values to run it non-interactively:

```sh
wuko run --file .wuko/workflows/choice-and-table/wuko.yaml \
  --var environment=staging --var access_mode=read-only
```

`vault-secrets` resolves a vault reference only when the Bitwarden CLI is installed and
unlocked, and otherwise prints the state it found and skips the lookup. Point it at an item of
your own:

```sh
wuko run --file .wuko/workflows/vault-secrets/wuko.yaml \
  --var item=GitHub \
  --var password_reference=bw://password/GitHub \
  --var username_reference=bw://username/GitHub
```

`scripted-pty` runs headlessly by default — scripted PTYs use a 24x80 terminal and need no
file-backed terminal. Its last step hands the console to you and needs a real terminal:

```sh
wuko run --file .wuko/workflows/scripted-pty/wuko.yaml --var handoff=true
```

## Plugins

The marketplace also publishes executable plugins. Plugins are persistent programs that
exchange newline-delimited JSON with Wuko over stdin and stdout and contribute namespaced steps,
executors, and helpers.

| Plugin | Provides | Platforms |
| --- | --- | --- |
| `cue` | The `cue.eval` step for typed CUE evaluation and policy validation | darwin and linux on amd64 and arm64 |
| `local-notifier` | The `local-notifier.notify` step for terminal or desktop notifications | darwin and linux on amd64 and arm64 |
| `hello` | The `hello.uppercase` step, the `hello.local` executor, and the `hello_slug` helper | darwin and linux on amd64 and arm64 |

```sh
# Interactive picker over every compatible plugin
wuko plugin install --global https://github.com/up2jj/wuko-marketplace

# Non-interactive
wuko plugin install --global --package hello https://github.com/up2jj/wuko-marketplace

# Install CUE evaluation plus its runnable example
wuko plugin install --global --package cue https://github.com/up2jj/wuko-marketplace
wuko install --package cue-eval https://github.com/up2jj/wuko-marketplace

# Install local notifications plus its runnable example
wuko plugin install --global --package local-notifier https://github.com/up2jj/wuko-marketplace
wuko install --package local-notifier-demo https://github.com/up2jj/wuko-marketplace

wuko plugin uninstall --global hello
```

To install a specific CUE plugin release globally, address its published manifest through an exact
marketplace tag or commit. Wuko does not resolve a separate `--version` flag; the Git ref selects
the release. Use `--reinstall` when replacing an existing global installation:

```sh
wuko plugin install --global --reinstall \
  github:up2jj/wuko-marketplace@cue-v0.1.0:plugins/cue/plugin.json

# A full commit SHA is the strongest pin:
wuko plugin install --global --reinstall \
  github:up2jj/wuko-marketplace@<commit-sha>:plugins/cue/plugin.json
```

The selected `plugin.json` declares the informational plugin version and pins every platform
archive by SHA-256. A tag must be published to GitHub before it can be used as an installation ref.

Installation downloads only the current-platform archive, verifies the manifest digest and the
archive digest, extracts regular files safely, performs the pure `initialize` handshake, and
publishes the installation atomically. It never calls `plugin.start` and never executes a binary
during import or validation. The picker lists only plugins that support the running OS and
architecture.

Once installed, a namespaced reference needs no declaration:

```yaml
steps:
  - id: shout
    type: hello.uppercase
    with: {value: hello}
```

`local-notifier.notify` accepts a required `message`, an optional `title` that defaults to the
workflow name, and `delivery: auto|terminal|system`. Terminal delivery streams a plain
`<title>: <message>` line. System delivery uses `/usr/bin/osascript` on macOS and `notify-send` on
Linux, where a running desktop notification daemon is also required. `system` fails when the
notifier is unavailable; the default `auto` mode falls back to the terminal instead. The step
reports the actual `delivery` plus a boolean `fallback` output. Operating-system notification
settings may suppress a banner even after a successful submission.

```yaml
steps:
  - id: notify
    type: local-notifier.notify
    with:
      message: Build completed
      delivery: auto
```

`cue.eval` accepts either inline `source` or one workflow-relative `.cue` file and publishes the
concrete top-level CUE `output` at `.steps.<id>.value`:

```yaml
steps:
  - id: plan
    type: cue.eval
    with:
      source: |
        #Port: int & >=1 & <=65535
        output: {
          service: "api-\(wuko.vars.environment)"
          port: #Port & wuko.vars.port
        }

  - id: policy
    type: cue.eval
    with:
      file: policy.cue
```

The read-only `wuko` snapshot exposes inputs, variables, environment, earlier step and dependency
outputs, workflow and run directories, and attempt metadata. This makes the step useful for schema
validation, policy enforcement, defaults and unification, normalization, generated matrices, and
typed transformations. Version 0.1.0 intentionally evaluates one self-contained source: native CUE
workflow files, CUE module loading, specialized steps such as `cue.validate`, and CUE-backed helper
functions remain future extensions.

A workflow that must pin an exact release declares it instead, which is also the only way a
plugin contributes template helpers:

```yaml
plugins:
  hello:
    source: github:up2jj/wuko-marketplace@<ref>
    sha256: <digest of plugins/hello/plugin.json>
```

**Marketplace repositories distribute native executable code.** SHA-256 verification protects file
integrity; it does not establish publisher identity, make a plugin safe, or provide a sandbox.
Install plugins only from maintainers you trust.

## Changes with no package of their own

Not every recent addition is something a workflow file can declare, so these have no package:

- **Static data reference validation.** Variable and step references in templates are now checked
  before any step is constructed, not when the consuming step starts. A variable must be declared
  under `vars:`, supplied by the invocation, or written by an earlier step that names what it
  assigns; a step ID must be visible where the template sits. `concurrent-dag` relies on this —
  reading a variable that is only reachable through a `needs` edge the child does not declare fails
  up front. `lua` and `import_vars` name their variables only at run time and therefore end
  variable checking for the steps after them. Environment names are deliberately *not* checked,
  because the effective environment inherits the host process.
- **No clock outside the `time` step.** Expr's `now()` builtin is disabled and fails to compile
  with `unknown name now`. `recordable-time` shows the replacement.
- **The `multiplexer` reporter.** `wuko run <name> --reporter plain --reporter multiplexer`
  animates root progress in the detected tmux, cmux, or Herdr title, rendering frames such as
  `⠋ check · 3/8 · test` and leaving a final `✓`, `✗`, `■`, or `⏱`. It is a no-op without a
  detected multiplexer and an interactive stderr. Try it against `multiplexer-status`.
- **Reporter correlation identifiers.** Go integrations implementing `reporter.Reporter` now
  receive an invocation ID, run IDs, and step-run IDs through `reporter.Session`. These are
  deliberately *not* exposed to workflow templates, step environments, or GitHub output, and
  are separate from a step's user-definable `operation_id` idempotency key.
- **A workflow on standard input.** The exact path `-` reads one workflow snapshot from stdin:
  `wuko run --file - < wuko.yaml`. Relative resources resolve against the invocation's current
  directory. Piping a package example works: `wuko run --file - < .wuko/workflows/hello-wuko/wuko.yaml`.
- **Client-side Git hooks.** `wuko git hook init` writes `.wuko/git-hooks.yaml` plus shell-free
  example workflows, `wuko git hook install` installs the dispatchers (`--chain` preserves an
  existing hook and runs it first), and `wuko git hook status` reports their state. Bindings are
  version-controlled and may only name locally discovered workflows, so a manifest change alone
  can never make a hook fetch unreviewed code. A hook run gets a read-only `git` context —
  `.git.hook.name`, `.git.hook.args`, `.git.hook.stdin`, `.git.hook.payload`, and the absolute
  `.git.repository.root`. `commit-policy` is the shape of a `commit-msg` binding, and
  `git-history` shows the `git_diff_check` that a `pre-commit` or `pre-push` binding uses.
- **A canonical execution report.** A run can publish one machine-readable report — schema
  version, invocation and run IDs, workflow name, status, duration, per-status step counts,
  attempts, retries, polls, and declared outputs — for CI to archive instead of scraping logs.
- **Invocation environment loaders.** Repeatable `--env-loader auto|none|mise|asdf|direnv` loads
  the invocation environment before the workflow does. The loaders that actually changed
  something are listed in `.run.environment_loaders`, which is always a list and empty when none
  applied.
- **Execution provider contexts.** A read-only top-level context describes the system running the
  workflow, consistently across environment rendering, templates, Expr, Lua, actions, dependency
  workflows, attempts, scheduled runs, and lifecycle steps. GitHub Actions is the built-in
  provider and activates only when `GITHUB_ACTIONS` is exactly `true`, exposing `.github` with
  repository, actor, event, optional `pull_request`, `sha`, `ref`, run metadata, and the complete
  `payload`. A registered provider that is not active is *absent*, and references to it fail
  static validation rather than silently becoming an empty object — which is why no package here
  can demonstrate it locally. Treat every `payload` value as untrusted input.
- **Wuko as a composite GitHub Action.** A workflow can run through a composite action that
  exports a configurable token to the workflow and writes a per-step job summary. Actions can
  also be loaded from a directory in a GitHub repository.
- **Filesystem steps inside executor scopes.** `file` and `edit` now work against the filesystem
  of a Docker or devenv executor rather than only the host, and `docker` executors can supervise
  managed services inside the container. Both need a container, so neither has a package here.

## Repository layout

```
manifest.json                        # generated - never edit by hand
packages/<name>.tar.gz               # generated deterministic workflow archives
plugins/<ns>/plugin.json             # generated public plugin release manifest
plugins/<ns>/dist/*.tar.gz           # generated public platform archives
.wuko/workflows/<name>/wuko.yaml     # workflow package sources
.wuko/plugin-sources/<ns>/           # imported plugin releases, with build-only provenance
plugin-src/cue/                       # maintained source for the CUE plugin
plugin-src/local-notifier/            # maintained source for the local-notifier plugin
```

`.wuko/plugin-sources/` is deliberately *not* `.wuko/plugins/`, which is the local plugin
*installation* root: an imported release carries no executable, so sharing the directory would
break plugin discovery for that namespace in this repository and every directory below it.

`manifest.json` records a `source_sha256` (a digest of the package source tree) alongside the
archive `sha256`. Only the archive digest is verified at install time; the source digest is a
rebuild-freshness key that no other tool can recompute.

After changing or adding a workflow, regenerate both from the repository root:

```sh
wuko marketplace build
```

`build` discovers any directory under `.wuko/workflows/` holding a root `wuko.yaml`, packages
every imported release under `.wuko/plugin-sources/`, rebuilds only what changed, and names each
workflow package after its `name` field — so keep the directory name and the workflow `name`
identical. It preserves unchanged files and their timestamps, and removes a stale generated file
only when it still matches the digest previously recorded for it. Validate before building, and
use `--check` in CI to verify without writing:

```sh
wuko validate
wuko marketplace build
wuko marketplace build --check
```

Plugin releases are imported into the catalog rather than compiled by `marketplace build`. The CUE
and local-notifier plugin sources are maintained in this repository, while other plugin projects
may live beside it.
Do not edit `.wuko/plugin-sources/`, `plugins/`, or the plugin entries in `manifest.json` by hand.

### Releasing a plugin update

Choose the next semantic version before starting. Use a patch release for compatible fixes, a
minor release for compatible features, and a major release for incompatible contract changes.
For example, to release CUE plugin `0.1.1`, update and test the maintained source, build all four
platform artifacts, and import the completed release:

```sh
cd plugin-src/cue
just check
just build
just release 0.1.1

cd ../..
wuko marketplace plugin update cue ./plugin-src/cue
wuko marketplace build
wuko marketplace build --check
```

`just release` replaces `plugin-src/cue/plugin.json` and its ignored local `dist/` directory.
`plugin update` verifies every declared archive and digest, then atomically replaces the imported
release in `.wuko/plugin-sources/cue/`. The final build regenerates `plugins/cue/` and
`manifest.json`; it also rebuilds `packages/cue-eval.tar.gz` if the example workflow changed.

Before publishing, install the generated marketplace artifact and run the repository validation
and example workflow. `--reinstall` is required when that namespace is already installed globally:

```sh
wuko plugin install --global --reinstall ./plugins/cue/plugin.json
wuko validate
wuko run --file .wuko/workflows/cue-eval/wuko.yaml
```

Commit the maintained source and generated marketplace outputs together, then publish a namespaced
tag so consumers can pin the release:

```sh
git add plugin-src/cue .wuko/plugin-sources/cue plugins/cue \
  .wuko/workflows/cue-eval packages/cue-eval.tar.gz manifest.json README.md
git commit -m "feat(cue): release v0.1.1"
git tag cue-v0.1.1
git push origin main
git push origin cue-v0.1.1
```

Use `plugin add --description "..." SOURCE` only for a namespace's first import. For every later
release, use `plugin update NAMESPACE SOURCE`; it preserves the catalog description. Both commands
accept the same local, HTTPS, and pinned `github:` sources as direct installation. They download
every declared platform archive, validate each digest and safe executable structure, and commit the
import atomically without executing any binary. Failed validation leaves the previous import
intact.
