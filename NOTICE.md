# NOTICE

This repository is a derivative of **opencode-cc**, originally created by
Kiowx (https://github.com/Kiowx/opencode-cc) and later forked by wraient
(https://github.com/wraient/opencode-cc). Both are MIT licensed; the original
copyright notice is preserved in `LICENSE`.

The project is maintained here as an independent repository. It is not a GitHub
fork, carries no upstream commit history, and diverges substantially from the
upstream project.

## What this repository adds on top of upstream

- **Native free-tier transport** (`internal/native/`): an owned Bun sidecar on an
  ephemeral loopback port with a per-process token, replacing the retired
  `:18790` helper. Free-tier traffic keeps a genuine TLS/HTTP2 client instead of
  going through the Go relay, which is what the free tier gates on.
- **Anthropic Messages compatibility** for Claude Code: streaming translation,
  tool-call recovery, reasoning replay, effort clamping, and window metadata.
- **Tool-call reliability work**: nameless-call recovery, toolless/aborted
  resampling, stall forensics, tool-name sanitisation and restoration.
- **Free-tier quota recovery**: WARP egress pool, session-gate handling,
  descending-timestamp session synthesis, cooldown propagation.
- **Deployment and diagnostics** in `docs/` and `README_DEPLOY.md`, describing
  the local CPA + opencode-cc + client topology used in production.

## Upstream components retained

The HTTP proxy, admin panel, key store, OpenAI/Responses bridges and the Vite
dashboard originate upstream and remain under the MIT license above. Files that
are unmodified upstream work keep their original authorship; the commit history
of this repository is intentionally a single squashed import, so per-line
attribution lives in this NOTICE rather than in git history.
