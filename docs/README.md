# Docs

Current, authoritative docs (describe the system as built):

- [DESIGN.md](DESIGN.md): architecture, the DRM toolchain, data model, OPDS
  contract, on-disk layout, deployment design, resolved decisions.
- [DEPLOY.md](DEPLOY.md): deploying the compose stack on any Docker host (with
  notes for a Proxmox LXC), the release flow, and first-run setup.
- [THEMES.md](THEMES.md): the CSS theming framework and how to add a theme (the
  token contract, with a Hot Dog Stand worked example).

[proposals/](proposals/): designed but **not implemented**. Specs to build from
if/when the feature is wanted; nothing here is wired into the running system.

- [proposals/KOBO_IMPORT.md](proposals/KOBO_IMPORT.md): a host-side helper to
  import Kobo books (Obok-based).
- [proposals/AUDIOBOOK_SUPPORT.md](proposals/AUDIOBOOK_SUPPORT.md): Audible
  `.aax` import + DRM removal (a second sidecar), an audio format, and a browser
  player with chapters.
