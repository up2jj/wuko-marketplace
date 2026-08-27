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
| [`recover-and-rollback`](.wuko/workflows/recover-and-rollback/wuko.yaml) | The `try`/`catch` control, the structured `error` root, and `recovered` | unreleased |
| [`race-a-monitor`](.wuko/workflows/race-a-monitor/wuko.yaml) | The `cancel_on` control, its result contract, and workflow-level `outputs:` | unreleased |
| [`probe-exit-codes`](.wuko/workflows/probe-exit-codes/wuko.yaml) | `shell.allowed_exit_codes` plus `stdout`/`stderr` capture policies | v0.9.0 |
| [`scripted-pty`](.wuko/workflows/scripted-pty/wuko.yaml) | `shell.interactions` (static and `expr`), `sensitive` sends, and `terminal` styling | v0.10.0 / unreleased |
| [`choice-and-table`](.wuko/workflows/choice-and-table/wuko.yaml) | `tui_table`, computed `tui_choice` `*_expr` properties, and `auto_select_single` | v0.9.0 / unreleased |
| [`lua-typed-args`](.wuko/workflows/lua-typed-args/wuko.yaml) | `lua` argument expressions and the `wuko.*` runtime snapshot roots | unreleased |
| [`multiplexer-status`](.wuko/workflows/multiplexer-status/wuko.yaml) | The `multiplexer` step for tmux, cmux, and Herdr, including tab scope and title restore | unreleased |

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
```

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

## Two features with no YAML surface

Not every recent addition is something a workflow file can declare, so neither has a package:

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
