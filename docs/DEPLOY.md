# Deploying on Proxmox

The deploy target is a Proxmox host. macOS is for development only. The
shippable artifact is two container images; anything that runs Docker can run
the stack. This guide covers the recommended path: an **LXC container running
Docker**, pulling prebuilt **multi-arch images from GHCR**.

## 1. Build path: GHCR (CI builds, the host pulls)

CI (`.github/workflows/release.yml`) builds `linux/amd64` + `linux/arm64`
images on every push to `main` and on version tags, and pushes them to GHCR:

- `ghcr.io/<owner>/<repo>-library`
- `ghcr.io/<owner>/<repo>-drm-sidecar`

Tags: `latest` and `main` track the default branch; `vX.Y.Z` tags are pinned
releases. The Proxmox host never builds; it pulls.

If the GHCR packages are private, create a classic PAT with `read:packages` and
`docker login ghcr.io` on the host once.

## 2. The Proxmox LXC

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
containers. Put them somewhere persistent on the LXC (or a bind-mounted Proxmox
dataset):

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
IMAGE=ghcr.io/<owner>/<repo> TAG=latest \
LIBRARY_BASE_URL=http://<lan-ip>:8080 \
docker compose -f docker-compose.prod.yml up -d
```

`LIBRARY_BASE_URL` must be the host's LAN address so the OPDS feed emits links
the Xteink X4 can fetch. The repo's `make prod-up` / `make prod-deploy` targets
wrap these commands if you have the repo checked out on the host.

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
  drm-sidecar python /opt/setup.py
```

It prompts for the AdobeID, password, and ADE version, and writes the same files
into `secrets/`. Re-run either method to re-authorize.

## 6. Operating it

- **Add the X4:** point its OPDS client at `http://<lan-ip>:8080/opds`.
- **Browser library:** `http://<lan-ip>:8080/`.
- **Import:** drop or upload `.acsm` / `.epub`; the watcher fulfills + decrypts.
- **Update:** `docker compose -f docker-compose.prod.yml pull && ... up -d`
  (or `make prod-deploy`). State in `data/` / `secrets/` survives.
- **Logs:** `docker compose -f docker-compose.prod.yml logs -f`.

## 7. Notes specific to this deploy

- **Clock:** unlike the macOS Podman VM, a Linux LXC keeps real time, so the
  ADEPT `E_ADEPT_REQUEST_EXPIRED` clock-skew issue does not occur here. No
  `time-sync` step is needed.
- **Arch:** the images are multi-arch, so the same compose works on an amd64 or
  arm64 Proxmox host.
- **Other runtimes:** nothing here is Proxmox-specific beyond the LXC nesting
  note. Any Docker host (a VM, another NAS, a bare Linux box) runs the same prod
  compose.
