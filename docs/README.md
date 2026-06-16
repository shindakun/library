# Docs

Current, authoritative docs (describe the system as built):

- [DESIGN.md](DESIGN.md): architecture, the DRM toolchain, data model, OPDS
  contract, on-disk layout, deployment design, resolved decisions.
- [DEPLOY.md](DEPLOY.md): deploying the compose stack on any Docker host (with
  notes for a Proxmox LXC), the release flow, and first-run setup.

[proposals/](proposals/): designed but **not implemented**. Specs to build from
if/when the feature is wanted; nothing here is wired into the running system.

- [proposals/KOBO_IMPORT.md](proposals/KOBO_IMPORT.md): a host-side helper to
  import Kobo books (Obok-based).
- [proposals/METADATA_EDITING.md](proposals/METADATA_EDITING.md): in-browser
  metadata editing.
- [proposals/COMIC_SUPPORT.md](proposals/COMIC_SUPPORT.md): CBZ/CBR comic
  import, cataloging, and a browser comic viewer.
- [proposals/IMPORT_PROGRESS.md](proposals/IMPORT_PROGRESS.md): import-job
  tracking, live SSE progress, and a dedicated import page (prerequisite for
  comic CBR->CBZ conversion).
