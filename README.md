# Wuko workflow marketplace

A version-1 archive marketplace of runnable [Wuko](https://github.com/up2jj/wuko) workflows.
Each package demonstrates one capability of the engine, and every example is self-contained:
POSIX shell builtins and Wuko's own steps only, with no network calls, containers, coding
agents, or external CLIs.

## Install

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
| [`lua-typed-args`](.wuko/workflows/lua-typed-args/wuko.yaml) | `lua` argument expressions and the `wuko.*` runtime snapshot roots | v0.11.0 |
| [`multiplexer-status`](.wuko/workflows/multiplexer-status/wuko.yaml) | The `multiplexer` step for tmux, cmux, and Herdr, including tab scope and title restore | v0.11.0 |
| [`concurrent-dag`](.wuko/workflows/concurrent-dag/wuko.yaml) | Sibling `needs` edges inside `concurrent`, ancestor state, and descendant skipping | unreleased |
| [`scoped-environments`](.wuko/workflows/scoped-environments/wuko.yaml) | `env:` blocks: nesting, restoration on exit, and what `defer` and `finally` see | unreleased |
| [`structured-edit`](.wuko/workflows/structured-edit/wuko.yaml) | The `edit` step: JSONPath selection over JSON, YAML, and TOML with comments preserved | unreleased |
| [`durable-state`](.wuko/workflows/durable-state/wuko.yaml) | `key_value` `expr`, atomic `update`, `variable`, `default`, `prefix`, and `clear` | unreleased |
| [`run-once`](.wuko/workflows/run-once/wuko.yaml) | `once` blocks: keyed idempotency, replayed results, and `on_busy: wait` | unreleased |
| [`recordable-time`](.wuko/workflows/recordable-time/wuko.yaml) | The `time` step, `workflow.timezone`, and the pure `parseTime`/`addTime`/`formatTime` helpers | unreleased |

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
```

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

`scripted-pty` runs headlessly by default — scripted PTYs use a 24x80 terminal and need no
file-backed terminal. Its last step hands the console to you and needs a real terminal:

```sh
wuko run --file .wuko/workflows/scripted-pty/wuko.yaml --var handoff=true
```

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

## Repository layout

```
manifest.json                     # generated - never edit by hand
packages/<name>.tar.gz            # generated deterministic archives
.wuko/workflows/<name>/wuko.yaml  # package sources
```

`manifest.json` records a `source_sha256` (a digest of the package source tree) alongside the
archive `sha256`. Only the archive digest is verified at install time; the source digest is a
rebuild-freshness key that no other tool can recompute.

After changing or adding a workflow, regenerate both from the repository root:

```sh
wuko marketplace build
```

`build` discovers any directory under `.wuko/workflows/` holding a root `wuko.yaml`, rebuilds
only what changed, and names each package after the workflow's `name` field — so keep the
directory name and the workflow `name` identical. Validate before building:

```sh
wuko validate
```
