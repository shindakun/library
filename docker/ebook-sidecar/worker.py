#!/usr/bin/env python3
"""DRM sidecar worker.

A tiny always-on HTTP worker that the Go service drives. It runs the two
file-in / file-out transforms of the import pipeline and nothing else:

  POST /job {"op":"fulfill","input":"/work/x.acsm"}  -> encrypted .epub
  POST /job {"op":"decrypt","input":"/work/x.epub"}  -> clean .epub
  GET  /health                                        -> 200 if keys present

This is the ONLY component that touches /secrets, read-only. It uses:

  - acsm-calibre-plugin standalone scripts (libadobe*) for fulfillment
  - DeDRM_tools' ineptepub.py for ADEPT decryption

Both are pure Python (lxml + pycryptodome); no Calibre, no GUI.

Secrets layout (mounted read-only at /secrets, created once via setup, see
docs/DESIGN.md §4.1):
  /secrets/activation.xml
  /secrets/device.xml
  /secrets/devicesalt
  /secrets/adobekey_*.der    <- the account decryption key
"""

import glob
import json
import os
import shutil
import subprocess
import sys
import tempfile
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SECRETS = os.environ.get("SECRETS_DIR", "/secrets")
WORK = os.environ.get("WORK_DIR", "/work")
ACSM_TOOLS = os.environ.get("ACSM_TOOLS_DIR", "/opt/acsm")   # acsm-calibre-plugin/calibre-plugin
# ACSM_DEPS holds the bundled oscrypto/asn1crypto fork dirs (OpenSSL-3 capable);
# libadobe imports oscrypto, so these must be on the subprocess PYTHONPATH.
ACSM_DEPS = os.environ.get("ACSM_DEPS", "")
# DEDRM_PARENT is the directory CONTAINING the DeDRM_plugin package directory.
# ineptepub.py uses relative imports (from .utilities / from .argv_utils), so it
# must be run as a module: python -m DeDRM_plugin.ineptepub <key> <in> <out>
DEDRM_PARENT = os.environ.get("DEDRM_PARENT", "/opt")        # /opt/DeDRM_plugin/...
PORT = int(os.environ.get("PORT", "7000"))


def _acsm_env():
    """Environment for running the acsm standalone scripts: ACSM_TOOLS + the
    bundled oscrypto/asn1crypto on PYTHONPATH."""
    env = dict(os.environ)
    parts = [ACSM_TOOLS]
    if ACSM_DEPS:
        parts.extend(ACSM_DEPS.split(os.pathsep))
    if env.get("PYTHONPATH"):
        parts.append(env["PYTHONPATH"])
    env["PYTHONPATH"] = os.pathsep.join(parts)
    return env


def _der_key():
    keys = sorted(glob.glob(os.path.join(SECRETS, "adobekey_*.der")))
    return keys[0] if keys else None


def _have_activation():
    needed = ["activation.xml", "device.xml", "devicesalt"]
    return all(os.path.exists(os.path.join(SECRETS, f)) for f in needed)


def setup(mail, password, ade_version=1):
    """One-time Adobe authorization, driven non-interactively (e.g. from the web
    first-run form). Registers the device + account and exports the decryption
    key into SECRETS. Refuses if already configured. Mirrors setup.py / the CLI
    path, but feeds credentials in as arguments instead of TTY prompts.

    Requires SECRETS to be writable (the prod compose mounts it read-write so
    first-run setup can populate it).
    """
    if _have_activation() and _der_key():
        raise RuntimeError("already configured; setup is only available on first run")
    if not mail or not password:
        raise RuntimeError("AdobeID email and password are required")
    if int(ade_version) not in (1, 2):
        raise RuntimeError("ade_version must be 1 (ADE 2.0) or 2 (ADE 3.0)")

    env = _acsm_env()
    # The acsm scripts read credentials from module globals when set, falling
    # back to input() only when empty. We set them via a tiny driver run in a
    # temp dir (the scripts write their output files to CWD), then copy results
    # into SECRETS. Running as a subprocess keeps libadobe's global state out of
    # this long-lived server process.
    driver = (
        "import runpy, sys\n"
        "sys.argv=['register']\n"
        "import register_ADE_account as r\n"
        "r.VAR_MAIL=%r; r.VAR_PASS=%r; r.VAR_VER=%d\n"
        "r.main()\n"
    ) % (mail, password, int(ade_version))

    with tempfile.TemporaryDirectory(dir=WORK) as td:
        # 1. Register + activate -> activation.xml, device.xml, devicesalt.
        proc = subprocess.run(
            [sys.executable, "-c", driver],
            cwd=td, env=env, capture_output=True, text=True, timeout=120,
        )
        reg_log = proc.stdout + proc.stderr
        produced = [f for f in ("activation.xml", "device.xml", "devicesalt")
                    if os.path.exists(os.path.join(td, f))]
        if len(produced) != 3:
            raise RuntimeError("Adobe registration failed:\n" + reg_log)
        for f in produced:
            shutil.copy(os.path.join(td, f), os.path.join(SECRETS, f))

        # 2. Export the account decryption key -> adobekey_*.der. The key script
        #    does its own signIn, so it needs the credentials; VAR_MAIL/VAR_PASS/
        #    VAR_VER are all module globals (verified against the script), so set
        #    them and main() runs fully non-interactively.
        key_driver = (
            "import sys\n"
            "sys.argv=['getkey']\n"
            "import get_key_from_Adobe as g\n"
            "g.VAR_MAIL=%r; g.VAR_PASS=%r; g.VAR_VER=%d\n"
            "g.main()\n"
        ) % (mail, password, int(ade_version))
        for f in produced:
            shutil.copy(os.path.join(SECRETS, f), os.path.join(td, f))
        kproc = subprocess.run(
            [sys.executable, "-c", key_driver],
            cwd=td, env=env, capture_output=True, text=True, timeout=120,
        )
        key_log = kproc.stdout + kproc.stderr
        ders = [f for f in os.listdir(td) if f.endswith(".der")]
        if not ders:
            raise RuntimeError("Adobe key export failed:\n" + key_log)
        for f in ders:
            shutil.copy(os.path.join(td, f), os.path.join(SECRETS, f))

    return {"activation": _have_activation(), "key": _der_key() is not None}


def fulfill(input_path):
    """Fulfill an .acsm into an (encrypted) epub using the standalone fulfill.py.

    fulfill.py reads activation files from its CWD and writes "<Title>.epub" to
    its CWD, so we run it in a private temp dir seeded with the secrets.
    """
    if not _have_activation():
        raise RuntimeError("no Adobe activation in /secrets (run one-time setup: `make drm-setup`)")

    with tempfile.TemporaryDirectory(dir=WORK) as td:
        for f in ("activation.xml", "device.xml", "devicesalt"):
            shutil.copy(os.path.join(SECRETS, f), os.path.join(td, f))
        acsm_local = os.path.join(td, "in.acsm")
        shutil.copy(input_path, acsm_local)

        proc = subprocess.run(
            [sys.executable, os.path.join(ACSM_TOOLS, "fulfill.py"), acsm_local],
            cwd=td, env=_acsm_env(), capture_output=True, text=True, timeout=240,
        )
        log = proc.stdout + proc.stderr
        produced = [p for p in glob.glob(os.path.join(td, "*"))
                    if p.lower().endswith((".epub", ".pdf"))]
        if not produced:
            raise RuntimeError("fulfill produced no book\n" + log)

        src = produced[0]
        fmt = "epub" if src.lower().endswith(".epub") else "pdf"
        # Move out of the temp dir before it's cleaned up.
        out = os.path.join(WORK, os.path.basename(src))
        shutil.move(src, out)
        return out, fmt, log


def decrypt(input_path):
    """Strip ADEPT DRM from an epub via DeDRM's ineptepub.py and the .der key."""
    der = _der_key()
    if not der:
        raise RuntimeError("no adobekey_*.der in /secrets (run one-time setup: `make drm-setup`)")

    base = os.path.splitext(os.path.basename(input_path))[0]
    out = os.path.join(WORK, base + ".clean.epub")

    # Run as a module from DEDRM_PARENT so the relative imports (from .utilities,
    # from .argv_utils) resolve. ineptepub.py ALSO does a non-relative
    # `from zeroedzipinfo import ZeroedZipInfo` for a sibling module, so the
    # plugin dir itself must be on PYTHONPATH too. cli_main expects exactly
    # <keyfile.der> <inbook> <outbook> and returns 0 on success.
    env = dict(os.environ)
    plugin_dir = os.path.join(DEDRM_PARENT, "DeDRM_plugin")
    env["PYTHONPATH"] = plugin_dir + os.pathsep + env.get("PYTHONPATH", "")
    proc = subprocess.run(
        [sys.executable, "-m", "DeDRM_plugin.ineptepub", der, input_path, out],
        cwd=DEDRM_PARENT, env=env, capture_output=True, text=True, timeout=120,
    )
    log = proc.stdout + proc.stderr
    if proc.returncode != 0 or not os.path.exists(out):
        raise RuntimeError("decrypt failed (rc=%d)\n%s" % (proc.returncode, log))
    return out, log


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            ok = _have_activation() and _der_key() is not None
            self._send(200 if ok else 503, {
                "ok": ok,
                "activation": _have_activation(),
                "key": _der_key() is not None,
            })
        else:
            self._send(404, {"ok": False, "error": "not found"})

    def do_POST(self):
        if self.path == "/setup":
            self._handle_setup()
            return
        if self.path != "/job":
            self._send(404, {"ok": False, "error": "not found"})
            return
        try:
            n = int(self.headers.get("Content-Length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            op = req.get("op")
            inp = req.get("input")
            if not inp or not os.path.exists(inp):
                raise RuntimeError("input not found: %r" % inp)

            if op == "fulfill":
                out, fmt, log = fulfill(inp)
                self._send(200, {"ok": True, "output": out, "format": fmt, "log": log})
            elif op == "decrypt":
                out, log = decrypt(inp)
                self._send(200, {"ok": True, "output": out, "format": "epub", "log": log})
            else:
                self._send(400, {"ok": False, "error": "unknown op %r" % op})
        except Exception as e:
            self._send(500, {"ok": False, "error": str(e), "log": traceback.format_exc()})

    def _handle_setup(self):
        try:
            n = int(self.headers.get("Content-Length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            result = setup(req.get("mail", ""), req.get("password", ""),
                           req.get("ade_version", 1))
            self._send(200, {"ok": True, **result})
        except Exception as e:
            self._send(500, {"ok": False, "error": str(e)})

    def log_message(self, fmt, *args):
        sys.stderr.write("sidecar: " + (fmt % args) + "\n")


if __name__ == "__main__":
    os.makedirs(WORK, exist_ok=True)
    print(f"ebook-sidecar listening on :{PORT} (secrets={SECRETS}, work={WORK})", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
