# Deploying

The shippable artifact is two container images, so **anything that runs Docker
(or Docker-compatible compose) can run the stack**: a VM, a NAS, a bare Linux
box, or a Proxmox LXC. macOS is for development only.

This guide describes the general flow (GHCR images + the prod compose file), then
adds notes for the author's setup, a **Proxmox LXC running Docker**. Nothing here
is Proxmox-specific except the LXC section; on any other Docker host, skip it and
run the same compose.

## 1. Build path: GHCR (CI builds, the host pulls)

CI (`.github/workflows/release.yml`) builds `linux/amd64` + `linux/arm64`
images on every push to `main` and on version tags, and pushes them to GHCR
nested under the repo:

- `ghcr.io/shindakun/library/server`
- `ghcr.io/shindakun/library/sidecar`

Tags: `latest` and `main` track the default branch; `vX.Y.Z` tags are pinned
releases. The deploy host never builds; it pulls.

If the GHCR packages are private, create a classic PAT with `read:packages` and
`docker login ghcr.io` on the host once.

### Cutting a release

Releases are tag-driven, from the repo (not the deploy host):

```sh
make release VERSION=v0.1.0
```

This runs the checks, tags `v0.1.0`, and pushes `main` + the tag. CI then builds
the multi-arch images (stamped with the version) and creates the GitHub Release.
The deploy host runs a release by setting `TAG=v0.1.0` below.

## 2. The Proxmox LXC (skip on a non-Proxmox host)

If you are deploying on a plain Docker host (VM, NAS, bare Linux), skip this
section: you already have Docker. It applies only to running Docker inside a
Proxmox LXC.

Create a Debian (or Ubuntu) LXC. Docker-in-LXC needs nesting and keyctl:

- In the container's Options / features, enable **nesting=1** (and **keyctl=1**).
  Equivalent CLI on the Proxmox node: `pct set <ctid> -features nesting=1,keyctl=1`.
- An **unprivileged** LXC with nesting works for Docker in current Proxmox; if you
  hit cgroup or overlay errors, fall back to a privileged LXC or a small VM. The
  stack itself does not care which: it is just Docker.

Inside the LXC, install Docker (the convenience script or distro packages) and
the compose plugin.

## 3. Lay out persistent state

The stack keeps everything under two host directories, mounted into the
containers. Put them somewhere persistent on the host (on Proxmox, that can be a
bind-mounted dataset):

```text
/opt/library/
  docker-compose.prod.yml   copied from the repo (docker/)
  data/                     library/, import/, catalog.db, covers/
  secrets/                  Adobe activation + key (populated at setup)
```

`data/` and `secrets/` are created on first run if absent. Back up `secrets/`
once populated: it is the whole Adobe authorization.

## 4. Bring the stack up

From `/opt/library` (where the prod compose and state dirs live):

```sh
TAG=v0.1.0 \
LIBRARY_BASE_URL=http://<lan-ip>:8080 \
docker compose -f docker-compose.prod.yml up -d
```

`TAG` selects the release to run (`latest` tracks main; pin a `vX.Y.Z` for a
release). `REGISTRY` defaults to `ghcr.io/shindakun/library`; override it only if
you forked. `LIBRARY_BASE_URL` must be the host's LAN address so the OPDS feed
emits links the Xteink X4 can fetch. The repo's `make prod-up` / `make
prod-deploy` targets wrap these commands if you have the repo checked out on the
host.

The prod compose (`docker/docker-compose.prod.yml`) differs from the dev
`docker-compose.yml`: it pulls images instead of building, and drops the
macOS/Podman-only bits (userns keep-id, SELinux relabels). It mounts `secrets/`
read-write so the web first-run setup can populate it.

## 5. First-run setup (Adobe authorization)

The DRM pipeline needs a one-time Adobe authorization (activation + key) written
into `secrets/`. There are two ways; both run the same upstream registration and
end with the same files in `secrets/`.

### Web form (default, headless-friendly)

On first run, with `secrets/` empty, the library page shows a **setup form**
instead of the catalog. Open `http://<lan-ip>:8080/`, enter a fresh / throwaway
AdobeID, password, and ADE version (2.0 is the default), and submit. The web
service forwards the credentials to the sidecar, which registers with Adobe and
writes `secrets/`. After that the form disappears and the endpoint refuses.

Credentials travel over your LAN as plain HTTP to the local sidecar and are used
only to register; they are not stored (only the resulting activation files and
`.der` key are kept). Keep the service LAN-only.

### CLI (the proven fallback)

If the web form ever fails, or you prefer a shell, run the interactive setup
against the sidecar image directly:

```sh
docker compose -f docker-compose.prod.yml run --rm -it \
  ebook-sidecar python /opt/setup.py
```

It prompts for the AdobeID, password, and ADE version, and writes the same files
into `secrets/`. Re-run either method to re-authorize.

## 6. Operating it

- **Add a reader:** point any OPDS client (the X4, or another e-reader) at
  `http://<lan-ip>:8080/opds`.
- **Browser library:** `http://<lan-ip>:8080/`.
- **Import:** drop or upload `.acsm` / `.epub`; the watcher fulfills + decrypts.
- **Update:** `docker compose -f docker-compose.prod.yml pull && ... up -d`
  (or `make prod-deploy`). State in `data/` / `secrets/` survives.
- **Logs:** `docker compose -f docker-compose.prod.yml logs -f`.

## 7. Notes

- **Clock:** a real Linux host (including an LXC) keeps real time, so the ADEPT
  `E_ADEPT_REQUEST_EXPIRED` clock-skew issue (which only affects the macOS Podman
  dev VM) does not occur in production. No `time-sync` step is needed.
- **Arch:** the images are multi-arch, so the same compose works on an amd64 or
  arm64 host.
- **Portability:** only §2 is Proxmox-specific (the LXC nesting note). Any Docker
  host (a VM, a NAS, a bare Linux box) runs the same prod compose unchanged.
