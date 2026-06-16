# Spec: Kobo import helper (`tools/kobo-import`)

Status: **proposed, not implemented.** This document is the implementation spec
for adding Kobo-book ingestion to the library. Everything below was verified
against the real tools and the user's actual Kobo library on 2026-06-16; build
from this if/when we decide to add it.

## 1. Why this is separate from the server

The library server (Go service + Python DRM sidecar) handles **Adobe ADEPT** DRM
via the `.acsm` → fulfill → `ineptepub` pipeline. Kobo books use a **different**
DRM scheme that ADEPT tooling cannot touch. Removing Kobo DRM needs Obok
(`noDRM/DeDRM_tools`, `Obok_plugin/obok`).

Obok derives the decryption key, for desktop-app libraries, as:

```text
deviceid = sha256(hash + macaddr)     # macaddr = MAC addresses of THIS machine
userkey  = sha256(deviceid + userid)  # userid read from Kobo.sqlite
```

(Verified in `Obok_plugin/obok/obok.py`, `__getuserkeys` / `__getmacaddrs`.)

**Consequence (the load-bearing constraint):** the key is bound to the MAC
addresses of the machine running Kobo Desktop. Obok therefore **must run on that
machine** (the user's Mac). Copying `Kobo.sqlite` to the Proxmox server and
running Obok there fails: the server's MACs differ, the deviceid won't match,
decryption produces garbage. This was the decisive reason the helper is host-side
and not part of the containerized stack.

### Topology

- **Mac** (has Kobo Desktop): runs `tools/kobo-import`. Has `Kobo.sqlite`, the
  `kepub/` files, and produces the valid keys.
- **Proxmox box** (runs the library in Podman): has the `import/` folder. No Kobo
  anything.
- **Transport decision (user, 2026-06-16):** the helper writes decrypted epubs to
  a **local output directory on the Mac only**. The user moves them to the remote
  `import/` by whatever means they already use. The helper makes **no network
  assumptions** (no mounts, no SSH). Keep it that way unless the user asks
  otherwise; a future `--out` pointing at a mounted share is a trivial extension.

Once a decrypted (DRM-free) epub lands in the server's `import/`, the **existing
direct-import path** absorbs it with zero server changes (the pipeline detects no
ADEPT DRM and imports directly). The content-hash **dedup** already built will
skip any Kobo book already in the library (e.g. a title present from both sources).

## 2. Verified facts (do not re-derive; confirmed this session)

- **Use the plugin module, not the standalone script.** `Obok_plugin/obok/obok.py`
  is the maintained version (`__version__ = '10.0.9'`) and is Python-3-correct.
  The standalone `Other_Tools/Kobo/obok.py` is stale and crashes on Python 3
  (`self.newdb.write('\x01\x01')` writes str to a binary temp file → `TypeError`).
- **It works on the user's real library.** A probe against the live
  `~/Library/Application Support/Kobo/Kobo Desktop Edition/Kobo.sqlite` found the
  kobodir, extracted candidate userkeys, and listed the library by title. (Only
  one of the candidate keys is the valid one; Obok tries them per book.)
- **macOS DB path:**
  `~/Library/Application Support/Kobo/Kobo Desktop Edition/Kobo.sqlite`,
  with encrypted books under the sibling `kepub/` directory.
- **Dependency:** `pycryptodome` (Obok imports `Cryptodome`/`Crypto`). Install in a
  throwaway venv: do NOT pollute global Python, and this repo has no other Python
  on the host side.

### 2.1 Decrypted-output format (verified by decrypting a real book)

A real Kobo book was decrypted with `decrypt_book` and the output inspected.
Findings:

- **DRM is fully removed.** Content files are real readable XHTML, not ciphertext.
  `decrypt_book` returned 0 ("Decryption succeeded").
- **The output imports cleanly through the existing direct-import path.** Dropped
  into the live `import/`, it logged "no Adobe DRM, importing directly" and landed
  in the catalog. So no server changes are needed to ingest Kobo output.
- **It renders in the epub.js reader.** The book is a valid EPUB (ZIP + OPF +
  XHTML); epub.js opens and displays it.
- **Two Kobo leftovers remain in the decrypted file, both benign:**
  - A root-level **`rights.xml`**, but it is NOT DRM. It is a vestigial Kobo stub:
    `<kdrm><timestamp>...</timestamp></kdrm>`, no keys, no encryption. Cosmetic.
  - **`koboSpan` markup throughout** the content: every run of text is wrapped in
    `<span class="koboSpan" id="kobo.N.N">...</span>` (13,072 of them in chapter 1
    alone). This is Kobo's pagination/stats instrumentation.
- **The koboSpans are inert for rendering.** The only CSS touching them is
  `.koboSpan { -webkit-text-combine: inherit; }` (an inline no-op), so they render
  as plain inline text. They bloat the file but do not break layout. Confirmed by
  the user: the decrypted book rendered correctly in the epub.js reader.

**Implication for the helper:** decrypted Kobo epubs work as-is (DRM-free, render
correctly). An OPTIONAL post-process could strip the koboSpan wrappers and the
vestigial `rights.xml` to produce a smaller, cleaner epub (this is what Calibre's
"KePub" / "Modify ePub" plugins do). Treat that as a nice-to-have, not required;
if added, verify rendering still works after stripping.

## 3. Obok API surface (what to call)

From `Obok_plugin/obok/obok.py`:

- `KoboLibrary(serials=[], device_path="", desktopkobodir="")`: constructing with
  no device/serials falls through to the desktop-app path (step 4) and loads
  `Kobo.sqlite`. Properties:
  - `.kobodir`: resolved Kobo dir (empty string if not found).
  - `.userkeys`: list of candidate keys (derived from MACs + DB userids).
  - `.books`: list of `KoboBook`.
  - `.close()`: cleans up the temp DB copy it makes.
- `KoboBook` instance vars: `volumeid` (UUID), `title`, `filename` (full path to
  the encrypted book), `type` (`"kepub"` or `"drm-free"`), `author`, `series`.
- `decrypt_book(book, lib)`: decrypts one book. **Caveat for the helper:** it
  writes `"<sanitized-title>.epub"` into the **current working directory** and
  prints status; returns nonzero-ish on failure. The helper must `chdir` into (or
  otherwise target) the output directory, or post-process the file into `--out`.
- `cli_main()` exists but is **interactive** (`raw_input`/`input` prompt) and only
  supports `--devicedir`/`--all`. Do not shell out to it for the desktop-app case;
  drive `KoboLibrary` + `decrypt_book` directly from the helper.

## 4. The helper to build

Location: `tools/kobo-import/` in this repo (host-side tool, clearly marked as NOT
part of the Podman stack). Suggested contents:

```text
tools/kobo-import/
  README.md          how to run it on the Mac; the MAC-binding caveat
  kobo_import.py     the driver (drives KoboLibrary + decrypt_book)
  requirements.txt   pycryptodome  (+ a pinned obok source, see §4.2)
```

### 4.1 Behavior

```text
kobo_import.py [--out DIR] [--all] [--list] [--title SUBSTR ...]

--list            print the Kobo library (number, title, author, type) and exit
--all             decrypt every kepub book
--title SUBSTR    decrypt books whose title contains SUBSTR (repeatable)
(no selection)    interactive numbered picker, like Obok's CLI but non-fragile
--out DIR         output directory (default ./kobo-out); created if missing
```

Flow:

1. Build `KoboLibrary([], "", "")`. If `.kobodir` is empty → error: "Kobo Desktop
   library not found; is the app installed and signed in on this machine?"
2. `--list`: enumerate `.books`, print, exit 0.
3. Select books per the flags (all / title filter / interactive).
4. For each selected book of `type == "kepub"`: run `decrypt_book(book, lib)` with
   CWD set to `--out` (use a `contextlib.chdir` or `os.chdir` + restore). Skip
   `type == "drm-free"` with a note (nothing to do).
5. `lib.close()`. Print a summary: N decrypted → `DIR`, M skipped/failed.
6. Tell the user to copy `DIR/*.epub` to the server's `import/`.

### 4.2 Sourcing the obok module

Two acceptable options; pick at implementation time:

- **Vendor** `Obok_plugin/obok/obok.py` (+ its sibling helpers it imports) into
  `tools/kobo-import/obok/`, pinned to a known DeDRM_tools release. Pro: hermetic,
  no clone at runtime. Con: carries third-party code in-repo (GPL, so keep the
  license/notice).
- **Fetch at setup**: a small `setup.sh` that clones DeDRM_tools at a pinned tag
  into a gitignored dir and points `PYTHONPATH` at `Obok_plugin/obok`. Pro: no
  vendored third-party code. Con: needs network at setup.

Either way: ensure `Obok_plugin/obok` is on `sys.path` (its modules use plain, not
relative, imports), and `pycryptodome` is installed in the venv.

### 4.3 Venv / setup

```sh
python3 -m venv .venv && .venv/bin/pip install pycryptodome
# then run: .venv/bin/python kobo_import.py --list
```

Do not install anything globally. `.venv/` and any fetched DeDRM clone must be
gitignored.

## 5. Verification checklist (when implemented)

Run against the real library and assert, not assume:

1. `--list` shows the library's books with titles (matches Kobo Desktop).
2. `--all --out /tmp/kobo-out` produces N `.epub` files.
3. Pick one output epub and confirm it is genuinely DRM-free: no
   `META-INF/rights.xml`, and a content file is real readable XHTML (same check
   used for the ADEPT pipeline).
4. Drop one output epub into the server's `import/` and confirm it imports via the
   **direct-import** path (log: "no Adobe DRM, importing directly") and lands in
   the catalog.
5. Drop a book already in the library and confirm the **dedup** skips it.

## 6. Risks / notes

- **MAC-binding fragility:** if the user changes network hardware or the Kobo app
  re-authorizes, the key set changes. The helper re-derives keys each run, so this
  is self-healing as long as it runs on the current Mac, but a previously-exported
  epub stays valid (it's already decrypted).
- **Kobo app updates** can change the DB schema or DRM; Obok tracks this upstream,
  so pin a recent DeDRM_tools release and be ready to bump it.
- **Not in the server stack:** never put Obok in the Podman sidecar; it cannot see
  the Mac's Kobo DB and the keys wouldn't match the server's MACs.
- **GPL:** Obok/DeDRM_tools is GPLv3. If vendored, keep the license and attribution.
- This is a **personal-use** tool for books the user owns, run locally; consistent
  with the rest of the project's posture.
