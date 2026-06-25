#!/usr/bin/env python3
"""Audiobook DRM sidecar worker.

A tiny always-on HTTP worker the Go service drives, the audiobook analog of the
ebook sidecar. It removes Audible DRM with ffmpeg, producing a clean, DRM-free,
chaptered .m4b, from either format:

  .aax   -> decoded with the account ACTIVATION BYTES (one secret for the whole
            account, stored in /secrets at setup).
  .aaxc  -> decoded with a PER-FILE key + IV read from a sibling "<name>.voucher"
            JSON that Audible ships beside the file; no account secret needed.

It is the ONLY component that touches the Audible activation bytes (kept in
/secrets).

  POST /job {"op":"decrypt","input":"/work/x.aax"}  -> clean .m4b in the work dir
  POST /job {"op":"decrypt","input":"/work/x.aaxc"} -> needs x.voucher beside it
  GET  /health                                       -> 200 if activation bytes present
  POST /setup {"bytes":"1A2B3C4D"}                   -> store pasted activation bytes
  POST /setup {"mail":...,"password":...}            -> retrieve via Audible login (Selenium)

Decryption is ffmpeg-native and lossless: the AAC stream and the embedded
chapters are copied through (`-c copy`), no re-encode. Long decrypts (a multi-
hundred-MB book is minutes of I/O) write progress to a sibling <output>.progress
JSON file so the Go side can poll it during the synchronous /job call.

Activation bytes are an account secret, retrieved ONCE at setup (login or paste)
and reused for every book on that account. They are 8 hex chars (4 bytes).
"""

import json
import os
import queue
import re
import secrets
import subprocess
import sys
import tempfile
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SECRETS = os.environ.get("SECRETS_DIR", "/secrets")
WORK = os.environ.get("WORK_DIR", "/work")
PORT = int(os.environ.get("PORT", "7100"))

# The activation bytes live in one tiny file under /secrets. 8 hex chars.
BYTES_FILE = os.path.join(SECRETS, "audible_activation_bytes")
HEX8 = re.compile(r"^[0-9a-fA-F]{8}$")


def _activation_bytes():
    """Return the stored activation bytes (8 hex chars) or None if not set."""
    try:
        with open(BYTES_FILE) as f:
            v = f.read().strip()
        return v if HEX8.match(v) else None
    except OSError:
        return None


def _store_bytes(value):
    """Validate + persist activation bytes. Raises on a bad value."""
    value = (value or "").strip().lower()
    if not HEX8.match(value):
        raise RuntimeError("activation bytes must be exactly 8 hex characters")
    os.makedirs(SECRETS, exist_ok=True)
    # Write atomically so a half-written file is never read as valid.
    fd, tmp = tempfile.mkstemp(dir=SECRETS)
    try:
        with os.fdopen(fd, "w") as f:
            f.write(value)
        os.replace(tmp, BYTES_FILE)
    except BaseException:
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise
    return value


def _probe_duration(path):
    """Total duration in seconds via ffprobe, or 0 if unknown (no key needed)."""
    try:
        out = subprocess.run(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "default=noprint_wrappers=1:nokey=1", path],
            capture_output=True, text=True, timeout=60,
        )
        return float(out.stdout.strip() or 0)
    except (ValueError, subprocess.SubprocessError):
        return 0.0


def _write_progress(progress_path, frac, detail):
    """Best-effort write of the convert fraction for the Go side to poll."""
    try:
        tmp = progress_path + ".tmp"
        with open(tmp, "w") as f:
            json.dump({"progress": round(frac, 4), "detail": detail}, f)
        os.replace(tmp, progress_path)
    except OSError:
        pass


def decrypt(input_path):
    """Decrypt a .aax or .aaxc to a clean .m4b in the work dir.

    Returns (output, log). Branches on the input format because the two carry
    DRM differently:

      .aax   -> AES key derived from the account ACTIVATION BYTES (one secret
                for the whole account, stored in /secrets at setup):
                ffmpeg -activation_bytes <hex>
      .aaxc  -> a PER-FILE AES key + IV that Audible ships in a sibling
                "<name>.voucher" JSON; no account secret is involved:
                ffmpeg -audible_key <hex> -audible_iv <hex>

    Either way decryption is lossless: the AAC audio + embedded chapters are
    copied through (-c copy), no re-encode.
    """
    ext = os.path.splitext(input_path)[1].lower()
    if ext == ".aaxc":
        key, iv = _voucher_key_iv(input_path)
        key_args = ["-audible_key", key, "-audible_iv", iv]
        wrong = "wrong voucher key/iv?"
    else:  # .aax (the default)
        abytes = _activation_bytes()
        if not abytes:
            raise RuntimeError("not configured: no Audible activation bytes (run setup)")
        key_args = ["-activation_bytes", abytes]
        wrong = "wrong activation bytes?"

    base = os.path.splitext(os.path.basename(input_path))[0]
    output = os.path.join(WORK, base + ".m4b")
    progress_path = output + ".progress"
    total = _probe_duration(input_path)

    cmd = [
        "ffmpeg", "-nostdin", "-hide_banner", "-y",
        *key_args,
        "-i", input_path,
        "-c", "copy", "-movflags", "+faststart",
        "-progress", "pipe:1", "-loglevel", "error",
        output,
    ]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    try:
        # ffmpeg -progress emits key=value lines; out_time_us / total = fraction.
        for line in proc.stdout:
            line = line.strip()
            if line.startswith("out_time_us=") and total > 0:
                try:
                    secs = int(line.split("=", 1)[1]) / 1_000_000
                    frac = max(0.0, min(secs / total, 1.0))
                    _write_progress(progress_path, frac, "%d/%d s" % (secs, total))
                except ValueError:
                    pass
        proc.wait()
        err = proc.stderr.read() if proc.stderr else ""
    finally:
        if proc.poll() is None:
            proc.kill()

    if proc.returncode != 0:
        # A wrong key makes ffmpeg fail here; surface it.
        _safe_remove(output)
        _safe_remove(progress_path)
        raise RuntimeError("ffmpeg decrypt failed (%s): %s" % (wrong, (err or "")[-500:]))
    if not os.path.exists(output) or os.path.getsize(output) == 0:
        _safe_remove(progress_path)
        raise RuntimeError("ffmpeg produced no output")

    _write_progress(progress_path, 1.0, "done")
    return output, (err or "")


def _voucher_key_iv(aaxc_path):
    """Read the per-file AES key + IV for an .aaxc from its sibling .voucher.

    Audible ships "<name>.voucher" beside the .aaxc: a JSON license response
    whose decrypted voucher holds the hex key/iv. The canonical layout (as
    written by audible-cli) nests them at
    content_license.license_response.{key,iv}; we also accept a flat
    {"key","iv"} for vouchers some tools store pre-extracted. Raises a clear
    error (no voucher / no key) so a voucherless .aaxc fails cleanly into
    import/failed/.
    """
    voucher_path = os.path.splitext(aaxc_path)[0] + ".voucher"
    if not os.path.exists(voucher_path):
        raise RuntimeError(
            "no voucher for this .aaxc: expected %s beside it. The .aaxc needs the "
            "per-file key/IV that Audible ships in the voucher; download both."
            % os.path.basename(voucher_path)
        )
    try:
        with open(voucher_path, "r") as f:
            data = json.load(f)
    except (OSError, ValueError) as e:
        raise RuntimeError("could not read voucher %s: %s" % (os.path.basename(voucher_path), e))

    # Canonical: content_license.license_response.{key,iv}
    lr = data.get("content_license", {}).get("license_response", {})
    key = lr.get("key") if isinstance(lr, dict) else None
    iv = lr.get("iv") if isinstance(lr, dict) else None
    # Fallback: a flat voucher dict.
    if not (key and iv):
        key = key or data.get("key")
        iv = iv or data.get("iv")
    if not (key and iv):
        raise RuntimeError(
            "voucher %s has no usable key/iv (expected content_license."
            "license_response.{key,iv})" % os.path.basename(voucher_path)
        )
    return key, iv


def _safe_remove(path):
    try:
        os.remove(path)
    except OSError:
        pass


def setup_paste(activation_bytes):
    """Store user-pasted activation bytes (the reliable primary path)."""
    if _activation_bytes():
        raise RuntimeError("already configured; setup is only available on first run")
    stored = _store_bytes(activation_bytes)
    return {"activation": True, "source": "paste", "bytes_len": len(stored)}


# --- Audible login (two-step: password, then OTP) ----------------------------
#
# The `audible` library's from_login() is one blocking call: when Amazon demands
# a 2FA one-time code it invokes otp_callback() and waits, then submits the code
# on the same session. A one-time code does NOT exist until the password step
# triggers it, so the UI can't collect it up front. We therefore run the login
# on a BACKGROUND THREAD and bridge the callback across two HTTP requests:
#
#   1. POST /setup {mail,password,marketplace}  -> starts the thread. If the
#      login reaches the OTP prompt, otp_callback blocks on a queue and we return
#      {otp_required: true, login_id}. The thread + session stay parked.
#   2. POST /setup {login_id, otp}              -> pushes the code into that
#      session's queue; the blocked callback returns it, the thread finishes the
#      login, fetches the activation bytes, and stores them.
#
# CAPTCHA still refuses (an image can't be solved in this flow); the user falls
# back to paste. Parked logins expire so an abandoned attempt can't leak a thread.

_PENDING = {}            # login_id -> _PendingLogin
_PENDING_LOCK = threading.Lock()
_PENDING_TTL = 300       # seconds a parked (waiting-for-OTP) login lives
_OTP_WAIT = 240          # seconds otp_callback blocks for the code before giving up


class _ChallengeRequired(Exception):
    """Internal: raised by the captcha/cvf callbacks to abort a login that needs
    interactive verification this flow can't satisfy (e.g. a CAPTCHA image)."""


class _PendingLogin:
    def __init__(self):
        self.otp_q = queue.Queue(maxsize=1)   # request thread -> blocked callback
        self.result = None                    # {"activation":...} on success
        self.error = None                     # str on failure
        self.done = threading.Event()         # set when the login thread finishes
        self.otp_wanted = threading.Event()   # set once otp_callback is waiting
        self.created = time.time()


def _prune_pending():
    """Drop parked logins older than the TTL (abandoned attempts)."""
    now = time.time()
    with _PENDING_LOCK:
        for lid in [k for k, p in _PENDING.items() if now - p.created > _PENDING_TTL]:
            _PENDING.pop(lid, None)


def setup_login(mail, password, marketplace="us"):
    """Start an Audible login. Returns either the final result (no 2FA needed) or
    {"otp_required": True, "login_id": ...} to be completed via setup_login_otp.
    """
    if _activation_bytes():
        raise RuntimeError("already configured; setup is only available on first run")
    if not mail or not password:
        raise RuntimeError("Audible email and password are required")
    _prune_pending()

    try:
        import audible  # noqa: F401  (probe the dep early, with a clear error)
    except ImportError as e:
        raise RuntimeError("login retrieval unavailable: the audible library is not installed (%s)" % e)

    pending = _PendingLogin()
    login_id = secrets.token_hex(8)

    def _otp_callback():
        # Called by the library when it hits the 2FA prompt. Signal the waiting
        # request, then block until setup_login_otp delivers a code (or we time
        # out, which aborts the login).
        pending.otp_wanted.set()
        try:
            return pending.otp_q.get(timeout=_OTP_WAIT)
        except queue.Empty:
            raise _ChallengeRequired()

    def _refuse(*_a, **_k):
        raise _ChallengeRequired()

    def _run():
        try:
            import audible
            auth = audible.Authenticator.from_login(
                username=mail,
                password=password,
                locale=marketplace,
                captcha_callback=_refuse,
                otp_callback=_otp_callback,
                cvf_callback=_refuse,
                approval_callback=_refuse,
            )
            abytes = auth.get_activation_bytes(extract=True)
            if not abytes:
                raise RuntimeError("login returned no activation bytes")
            stored = _store_bytes(abytes)
            pending.result = {"activation": True, "source": "login", "bytes_len": len(stored)}
        except _ChallengeRequired:
            pending.error = (
                "Audible asked for a CAPTCHA (or the OTP timed out). Paste your "
                "8-char activation bytes instead."
            )
        except Exception as e:
            pending.error = "Audible login failed: %s. Try the paste-bytes path." % (str(e)[:300])
        finally:
            pending.done.set()

    with _PENDING_LOCK:
        _PENDING[login_id] = pending
    threading.Thread(target=_run, daemon=True).start()

    # Wait briefly for the login to either finish (no 2FA) or reach the OTP
    # prompt. Whichever happens first decides the response.
    while True:
        if pending.done.wait(0.2):
            with _PENDING_LOCK:
                _PENDING.pop(login_id, None)
            if pending.error:
                raise RuntimeError(pending.error)
            return pending.result
        if pending.otp_wanted.is_set():
            return {"otp_required": True, "login_id": login_id,
                    "message": "Enter the 2FA / OTP code Audible just sent."}


def setup_login_otp(login_id, otp):
    """Deliver the OTP code to a parked login (step 2) and return its result."""
    if not otp:
        raise RuntimeError("OTP code is required")
    with _PENDING_LOCK:
        pending = _PENDING.get(login_id)
    if pending is None:
        raise RuntimeError("login session expired or unknown; start the login again")

    try:
        pending.otp_q.put_nowait(str(otp).strip())
    except queue.Full:
        raise RuntimeError("an OTP was already submitted for this login")

    if not pending.done.wait(60):
        raise RuntimeError("login timed out after submitting the OTP; try again")
    with _PENDING_LOCK:
        _PENDING.pop(login_id, None)
    if pending.error:
        raise RuntimeError(pending.error)
    return pending.result


# --- HTTP ---------------------------------------------------------------------

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
            ok = _activation_bytes() is not None
            self._send(200 if ok else 503, {"ok": ok, "activation": ok})
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
            if op == "decrypt":
                out, log = decrypt(inp)
                self._send(200, {"ok": True, "output": out, "format": "m4b", "log": log})
            else:
                self._send(400, {"ok": False, "error": "unknown op %r" % op})
        except Exception as e:
            self._send(500, {"ok": False, "error": str(e), "log": traceback.format_exc()})

    def _handle_setup(self):
        try:
            n = int(self.headers.get("Content-Length", 0))
            req = json.loads(self.rfile.read(n) or b"{}")
            if req.get("bytes"):
                result = setup_paste(req.get("bytes"))
            elif req.get("login_id"):
                # Step 2 of a two-step login: deliver the OTP code.
                result = setup_login_otp(req.get("login_id"), req.get("otp", ""))
            else:
                # Step 1: start the login (may come back needing an OTP).
                result = setup_login(
                    req.get("mail", ""),
                    req.get("password", ""),
                    req.get("marketplace", "us") or "us",
                )
            self._send(200, {"ok": True, **result})
        except Exception as e:
            self._send(500, {"ok": False, "error": str(e)})

    def log_message(self, fmt, *args):
        sys.stderr.write("audiobook-sidecar: " + (fmt % args) + "\n")


if __name__ == "__main__":
    os.makedirs(WORK, exist_ok=True)
    print(f"audiobook-sidecar listening on :{PORT} (secrets={SECRETS}, work={WORK})", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
