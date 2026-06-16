#!/usr/bin/env python3
"""One-time Adobe authorization + key export for the DRM sidecar.

Run this ONCE, interactively, to populate /secrets with:
  activation.xml, device.xml, devicesalt   (authorization)
  adobekey_<mail>_uuid_<uuid>.der           (decryption key)

Usage (inside the sidecar image, interactive):

  docker compose run --rm \\
      -v "$PWD/secrets:/secrets" \\
      drm-sidecar python /opt/setup.py

It wraps acsm-calibre-plugin's standalone register_ADE_account.py and
get_key_from_Adobe.py. Use a fresh / throwaway Adobe ID. Back up /secrets after.
"""

import os
import shutil
import subprocess
import sys
import tempfile

SECRETS = os.environ.get("SECRETS_DIR", "/secrets")
ACSM_TOOLS = os.environ.get("ACSM_TOOLS_DIR", "/opt/acsm")
ACSM_DEPS = os.environ.get("ACSM_DEPS", "")


def _env():
    env = dict(os.environ)
    parts = [ACSM_TOOLS]
    if ACSM_DEPS:
        parts.extend(ACSM_DEPS.split(os.pathsep))
    if env.get("PYTHONPATH"):
        parts.append(env["PYTHONPATH"])
    env["PYTHONPATH"] = os.pathsep.join(parts)
    return env


def run_in(td, script):
    return subprocess.run([sys.executable, os.path.join(ACSM_TOOLS, script)], cwd=td, env=_env())


def main():
    os.makedirs(SECRETS, exist_ok=True)
    print("=== Adobe authorization (use a throwaway Adobe ID) ===")
    with tempfile.TemporaryDirectory() as td:
        # register_ADE_account.py prompts for AdobeID/password/version and writes
        # activation.xml, device.xml, devicesalt into its CWD.
        if run_in(td, "register_ADE_account.py").returncode != 0:
            print("Registration failed.")
            sys.exit(1)
        for f in ("activation.xml", "device.xml", "devicesalt"):
            src = os.path.join(td, f)
            if not os.path.exists(src):
                print(f"Expected {f} not produced; aborting.")
                sys.exit(1)
            shutil.copy(src, os.path.join(SECRETS, f))

    print("=== Exporting account decryption key (.der) ===")
    with tempfile.TemporaryDirectory() as td:
        # get_key_from_Adobe.py prompts again and writes adobekey_*.der to CWD.
        if run_in(td, "get_key_from_Adobe.py").returncode != 0:
            print("Key export failed.")
            sys.exit(1)
        ders = [f for f in os.listdir(td) if f.endswith(".der")]
        if not ders:
            print("No .der key produced; aborting.")
            sys.exit(1)
        for f in ders:
            shutil.copy(os.path.join(td, f), os.path.join(SECRETS, f))

    print("\nDone. /secrets now holds your activation + key.")
    print("BACK UP /secrets somewhere safe (losing it burns an activation).")
    print("Contents:", sorted(os.listdir(SECRETS)))


if __name__ == "__main__":
    main()
