# wuko-plugin-local-notifier

`wuko-plugin-local-notifier` implements Wuko plugin protocol v1 and contributes the
`local-notifier.notify` step. It can stream a plain notification to the terminal, submit one to
the local desktop notification service, or try the desktop first and fall back to the terminal.

```yaml
- id: notify
  type: local-notifier.notify
  with:
    message: Build completed
    title: Release pipeline
    delivery: auto
```

`message` is required. `title` defaults to the current workflow name, then `Wuko` when the
workflow context has no name. `delivery` defaults to `auto` and accepts:

- `terminal` writes `<title>: <message>` through the step's stdout stream.
- `system` submits a desktop notification and fails if the platform notifier is unavailable.
- `auto` tries `system`, falls back to `terminal` on an unavailable or failed notifier, and fails
  only when the operation was canceled or the terminal stream cannot be written.

The step returns `delivery` (`system` or `terminal`) and `fallback` (a boolean). A successful
system submission does not guarantee that the operating system will display a banner: notification
permissions, focus modes, and desktop-session settings remain authoritative.

On macOS the plugin uses `/usr/bin/osascript`. On Linux it requires `notify-send` and a running
desktop notification daemon for `system` delivery. Arguments are passed directly without a shell.

For a release, run the checks and generate all four Darwin/Linux archives:

```sh
just check
just build
just release 0.1.0
```

Import the completed release from the marketplace root:

```sh
wuko marketplace plugin add \
  --description "Send local terminal or desktop notifications with automatic fallback" \
  ./plugin-src/local-notifier
wuko marketplace build
wuko marketplace build --check
```
