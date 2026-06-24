# Proposal: Audible audiobook import, DRM removal, and a player

Status: **proposed, not implemented.** Design + build plan for adding Audible
audiobooks (`.aax`, and the newer `.aaxc`) alongside epubs and comics: retrieve
the account's activation bytes once, decrypt + convert on import to a clean
chaptered file, catalog it, and play it in the browser with chapter navigation.
The import *flow* and the format-discriminator pattern reuse what comics already
established; the new parts are an Audible-specific sidecar, a long-running
convert step, an audio format in the catalog, and a player screen.

This is a back-catalog tool for audiobooks the user already owns, exactly like
the existing ADEPT path is for owned ebooks (see DESIGN.md). It is not a download
or sharing mechanism.

## 0. Decisions locked (read first)

- **Sidecar naming (by content type):** the existing `drm-sidecar` is renamed
  **`ebook-sidecar`**, and the new one is **`audiobook-sidecar`**. Both do DRM
  removal, so "drm-sidecar" was ambiguous; content-type names read clearly in the
  setup UI ("Ebook DRM" / "Audiobook DRM") and in compose/env. The env var
  `DRM_SIDECAR_URL` becomes `EBOOK_SIDECAR_URL`; the new one is
  `AUDIOBOOK_SIDECAR_URL`. (This rename is its own first build step; see §8.)
- **Independent optionality (one / other / both / none):** each sidecar is
  enabled by its own non-empty `*_SIDECAR_URL`, exactly like today's no-DRM mode.
  The Go service holds two optional clients; any combination runs. With neither,
  only comics + DRM-free epubs import.
- **Startup setup, per sidecar:** the first-run page shows an **Ebook DRM** setup
  section AND/OR an **Audiobook DRM** section, each appearing only if that sidecar
  is present (enabled) but not yet configured. Generalizes today's single
  AdobeID-only form. If a sidecar is disabled, its section never shows (mirrors
  how the whole form is hidden in no-DRM mode).
- **Audiobook setup offers BOTH paths:** Audible email/password (Selenium
  retrieval of activation bytes) OR pasting the 8-hex-char activation bytes
  directly. The paste path keeps the feature usable if Selenium breaks against an
  Audible change; login is the no-prior-tooling path.
- **v1 = `.aax`.** `.aaxc` (voucher-key) is a fast-follow (§4.3).
- **Sidecar lifecycle: always-running, lazy browser.** Sidecars start with the
  stack (not on demand, which would need Docker-socket access from the Go
  service). The audiobook sidecar idles as just the ffmpeg-capable server;
  Selenium/Chromium is a subprocess spawned only during setup and never kept
  warm (§3.1).

## 1. What an `.aax` is, and the one real wrinkle

An `.aax` is a standard MP4/M4B container (`major_brand=aax`) holding an AAC
audio stream plus **embedded chapter markers** and metadata (title, author,
narrator, cover art). `ffprobe` reads the container and chapters fine without any
key; the **audio stream is AAX-encrypted** and needs the account's
**activation bytes** (a 4-byte / 8-hex-char value) to decode.

```text
ffprobe sample.aax                 -> container, chapters, tags (no key needed)
ffmpeg -activation_bytes XXXXXXXX  -> decode the audio (needs the bytes)
```

Two facts that shape the design:

- **One activation-bytes value decrypts every book on that account.** It is the
  audiobook analog of the ADEPT account key: retrieved once at setup, reused for
  every import. There is no per-book fulfillment step like `.acsm`.
- **Decryption is ffmpeg-native.** Given the bytes,
  `ffmpeg -activation_bytes <hex> -i in.aax -c copy out.m4b` losslessly produces
  a clean, DRM-free M4B, copying the AAC stream and the chapters through. No
  re-encode needed (and re-encoding would be slow and lossy). The "conversion" is
  really a stream copy + remux; it is I/O-bound on a multi-hundred-MB file, which
  is why it still wants progress feedback (see §5).

### `.aaxc` (the newer format)

Newer Audible downloads are `.aaxc`: same container, but encrypted with a
**per-file key + IV** delivered in a sidecar `.voucher` JSON, not the account
activation bytes. ffmpeg decrypts it with `-audible_key`/`-audible_iv` instead.
v1 targets `.aax` (the user's test files are `.aax`); `.aaxc` is a near-identical
ffmpeg invocation and is noted in §4.3 as a fast-follow, gated on having a
`.voucher` alongside the file.

## 2. The activation-bytes wrinkle (setup)

Activation bytes are retrieved from Audible by `audible-activator`
(inAudible-NG): it drives a headless browser (Selenium) to log into the user's
Audible account, pulls the player-auth blob, and extracts the 8-hex-char value
(`common.extract_activation_bytes`). This is a **one-time setup**, mirroring the
existing Adobe first-run setup:

- The user has NOT extracted their bytes yet, so setup must be part of this
  feature, not a precondition.
- Like the Adobe setup, credentials are used once to obtain the bytes and are NOT
  stored; only the resulting `activation_bytes` are kept (in `/secrets`, like the
  ADEPT key).
- Selenium + a headless browser is a heavy, fragile dependency (a specific
  Chromium/driver pairing). It must NOT be bolted onto the existing ebook DRM
  sidecar, which is a carefully pinned acsm/DeDRM/oscrypto stack with no browser.

There is also a **manual fallback**: a user who already knows their activation
bytes (many do, from prior tooling) can paste the 8 hex chars directly, skipping
Selenium entirely. The setup form offers both: "retrieve via login" or "I already
have my activation bytes."

## 3. The architectural decision: a SECOND sidecar

Three options for where Audible decryption lives:

| Option | Trade-off |
| --- | --- |
| Extend the ebook sidecar | Pollutes the pinned epub stack with ffmpeg + Selenium + a browser; a Chromium update could break epub fulfillment. Rejected. |
| **New `audiobook-sidecar` (CHOSEN)** | Independent container: ffmpeg + Selenium, its own `/secrets` slice, its own job contract. The ebook sidecar is untouched. Clean blast radius, mirrors the existing "quarantine the messy part" pattern. |
| Do it in the Go service | The Go service must never import Python or shell heavy/fragile tooling; ffmpeg-as-subprocess is plausible but Selenium is not, and keeping all DRM in sidecars is the established boundary. Rejected. |

**Decision: a second sidecar, `audiobook-sidecar`** (the existing one becomes
`ebook-sidecar`, see §0). It is the audiobook analog of the ebook sidecar:
optional (only present if the user wants audiobooks), quarantined, and the only
component that touches the Audible activation bytes.

**Go-side client reuse (gap found in the code):** `internal/drm.Client` is mostly
generic, its HTTP transport (`do`, `health`, the `/job` `{op}` contract) is
format-neutral, but `Setup(mail, password, adeVersion)` and `Configured` are
Adobe-specific. So the audiobook side gets its own small client (e.g.
`internal/audible.Client`) that reuses the same job/health shape but has a
different `Setup` (login OR paste-bytes) and `Configured` semantic. Do NOT try to
force one `drm.Client` to serve both; the setup payloads genuinely differ. The
two clients are independent, nil-able fields on the importer/server.

Its job contract mirrors the existing one (`POST /job`, `GET /health`,
`POST /setup`):

```text
GET  /health                 200 if activation bytes are present
POST /setup {creds | bytes}  retrieve (Selenium) or accept pasted activation bytes
POST /job {"op":"decrypt", "input":"/work/x.aax"}  -> clean .m4b in the work dir
```

The decrypt op is just an ffmpeg invocation:
`ffmpeg -activation_bytes <bytes> -i <in.aax> -c copy -movflags +faststart
<out.m4b>`, reporting progress by parsing ffmpeg's `-progress` output (it emits
`out_time_us` / total, a real percentage, exactly what the job bar wants).

**Gap found in step 2: the existing `/job` contract is synchronous (one request,
one response, no progress stream).** That is fine for the seconds-long ebook
ops, but an audiobook decrypt is minutes. So: the audiobook sidecar's decrypt
runs ffmpeg with `-progress pipe:1`, parses `out_time_us` vs the known total
duration, and writes the fraction to a **sibling `<output>.progress` JSON file**
in the shared work dir as it goes. The job response stays synchronous (returns
when ffmpeg finishes), but the Go side can poll that progress file during the
call to drive the `/imports` bar (wired in build step 6). This keeps the simple
request/response contract while still giving a real percentage; no streaming
HTTP needed.

**Gap found in step 2: `audible-activator` uses the removed Selenium 3 API**
(`find_element_by_id`, `webdriver.Chrome(executable_path=...)`), so it does not
run as-is on Selenium 4. Combined with its own README admitting it breaks against
Audible site changes, the v1 plan is: **the paste-activation-bytes setup path is
the fully-built, reliable primary**, and the Selenium login retrieval is a
best-effort secondary (modernized to Selenium 4) that may need re-hardening over
time. The feature is fully usable via paste even if browser retrieval is down.
`extract_activation_bytes` is trivial (first 4 bytes of the activation blob,
byte-swapped to 8 hex chars) and is reimplemented cleanly, not copied from the
activator's py2 `common.py`.

Like the ebook sidecar, it is wired in compose, optional (an empty
`AUDIOBOOK_SIDECAR_URL` disables audiobooks, same pattern as the no-DRM mode),
and mounts the shared work dir.

### 3.1 Sidecar lifecycle (decided): always-running, lazy browser

Sidecars stay **always-running** (started by compose with the stack), NOT
started on demand. On-demand would save idle RAM but would require the Go service
to talk to the container runtime to start a sidecar, breaking the deliberate
"Go service has no Docker-socket access" hardening boundary, and would add
cold-start latency on every first import. Not worth it for a single-user deploy.

The real weight is the headless browser. So the rule: the **audiobook sidecar
idles as only the lightweight ffmpeg-capable Python HTTP server** (~50 MB,
~0 CPU). Selenium + Chromium is spawned as a **subprocess only during
`/setup`** (the one-time activation-bytes retrieval) and exits when setup
returns. It is never kept warm. Decryption jobs (`/job`) use ffmpeg only and
never touch the browser. This gives the savings of on-demand (no warm browser,
nothing running for a disabled sidecar) without the Docker-socket escalation.

Idle cost with both sidecars enabled is roughly ~100 MB RAM total and ~0 CPU;
each is independently optional, so you pay nothing for a sidecar you don't
enable ("none"). If true on-demand is ever wanted (RAM-constrained, rare
imports), the clean path is a compose `profiles` group started manually before a
batch, not auto-start from the Go service.

## 4. Import flow (what reuses, what is new)

`importable()` gains `.aax` (and later `.aaxc`). The pipeline branches on format,
exactly like the comic CBR branch:

```text
*.epub / *.acsm   -> existing ADEPT path (ebook-sidecar)            [unchanged]
*.cbz / *.cbr     -> comic path (convert CBR, no sidecar)         [unchanged]
*.aax             -> audible-sidecar decrypt -> clean .m4b -> import   [new]
```

The shared tail is identical to today: verify (probe the clean file), dedup by
content hash, name `Author/Title.m4b`, move into the library, index, archive the
original to `done/`. The clean M4B carries its chapters and tags, so no separate
metadata sidecar file is needed.

### 4.1 The audiobook package (`internal/audio`)

A sibling to `internal/epub` and `internal/comic`, pure-Go, **ffprobe-driven**
(no decryption in the Go service; it only reads the already-clean M4B):

```go
package audio

type Metadata struct {
    Title    string
    Authors  []string   // from the container tags
    Narrators []string
    Duration float64    // seconds
    HasCover bool
    Chapters []Chapter  // ordered
}

type Chapter struct { Title string; Start, End float64 } // seconds

func Read(path string) (*Metadata, error)          // ffprobe -show_format -show_chapters
func CoverImage(path string) ([]byte, string, error) // ffmpeg -an extract embedded cover
```

`Read` shells `ffprobe -print_format json -show_format -show_chapters` and parses
the JSON (chapters come straight out, as the probe above confirmed). This is the
ONE place the Go service touches ffmpeg, and only on a clean, owned file for
read-only metadata, so it does not need the sidecar.

### 4.2 Catalog + schema

The `format` column gains `"audio"` (the M4B). `formatForPath` maps `.m4b`
(and `.aax`/`.aaxc`, pre-conversion) to `"audio"`; `indexableExt` adds `.m4b`
(NOT `.aax`: only the converted clean file lands in the library, like CBR->CBZ).
The catalog's `readMetadata`/`coverImageFor` branch on `"audio"` to call
`audio.Read`/`audio.CoverImage`. `Book` already carries everything needed;
chapters are read live from the file by the player endpoint (not stored), since
they are intrinsic to the M4B.

### 4.3 `.aaxc` fast-follow

`.aaxc` import is the same path with a different ffmpeg key source: the sidecar
reads the per-file key/IV from the `.voucher` JSON that Audible ships beside the
`.aaxc` and invokes `ffmpeg -audible_key <k> -audible_iv <iv> ...`. It needs the
voucher present at import; document that, and reject an `.aaxc` with no voucher
into `failed/` with a clear reason.

## 5. Conversion is a long task (reuse the job infra)

Decrypting + remuxing a 600 MB audiobook is minutes of I/O. The import-job
registry + SSE stream + progress bar (built for comics) already model exactly
this: the sidecar's ffmpeg `-progress` output gives a real `done/total`
percentage, threaded through the existing `onProgress("converting", frac,
detail)` hook into the job, shown on `/imports`. No new progress machinery; the
audiobook decrypt is just another `onProgress`-reporting step in `pipeline()`,
the same shape as `convertCBR`.

One addition: these jobs are longer than comic conversions, so confirm the job
retention window and the sidecar HTTP timeout (`drm.Client` uses a 5-min timeout;
a long audiobook may exceed it). Make the audiobook client timeout generous
(e.g. 30 min) or switch the long ops to a poll/stream contract.

## 6. The player screen

epub.js and the comic viewer cannot play audio. Add a third reader surface, a
dependency-free **HTML5 `<audio>` player** with chapter navigation, selected by
`book.Format == "audio"` at `GET /read/{slug}` (same dispatch that already picks
`comic.html` vs `reader.html`).

- `GET /read/{slug}` renders `audio.html` for audiobooks.
- Endpoints:
  - `GET /book/{slug}/file` already serves the M4B bytes (with range requests,
    which `http.ServeFile`/`ServeContent` support, so seeking works).
  - `GET /book/{slug}/chapters` -> JSON chapter list (title + start/end seconds),
    read live via `audio.Read`.
- `audio.js`: one `<audio src=".../file">` element, a chapter list that seeks on
  click (`audio.currentTime = start`), current-chapter highlight driven by
  `timeupdate`, play/pause/skip, a scrubber, and playback-speed. Position is
  persisted to the existing `read_state` table (store seconds in `percent`/`cfi`,
  or add a `position` column), so resume works like the epub reader.
- Reuses `app.css`/`app.js` (theme, the standardized header). No framework; this
  is a handful of `<audio>` API calls.

MP3 chapter note: the user asked about MP3 chapters. The Audible path produces
M4B, which has native chapters (the player reads them via `/chapters`). If
plain-MP3 audiobooks are imported later, MP3 "chapters" are non-standard (ID3
`CHAP` frames, inconsistently present); treat a chapterless audio file as a
single-track book and only surface chapters when ffprobe finds them. M4B is the
first-class, reliable chapter source.

## 7. OPDS

OPDS 1.2 has an audiobook profile, but the verified target device (Xteink X4) is
an e-ink e-reader that does not play audio. So: emit audiobooks in the feed with
the audio acquisition type (`audio/mp4` / `audio/mpeg`) for audio-capable OPDS
clients, but treat the **browser player as the primary surface**, and flag X4
audiobook playback as not-applicable rather than a goal. Decide during build
whether to include audiobooks in the X4-facing feed at all (they would just fail
to open there).

## 8. Build order

1. (DONE) **Rename `drm-sidecar` -> `ebook-sidecar`** (its own self-contained step, no
   behavior change): the compose service + dir, `DRM_SIDECAR_URL` ->
   `EBOOK_SIDECAR_URL`, the Go field/var names where they read as generic "DRM"
   (`s.DRM`, `drmClient` -> ebook-specific), and docs (DESIGN/DEPLOY/README).
   Kept `internal/drm` as the package name (it IS the ebook DRM client; renaming
   the package was churn with no behavior value). `EBOOK_SIDECAR_URL` keeps
   `DRM_SIDECAR_URL` as a fallback so existing deploys don't break. Shipped +
   verified (epub import unchanged) before adding audiobooks.
2. (DONE) `audiobook-sidecar`: container (ffmpeg, stdlib Python), `/health`,
   `/setup` (paste-bytes built; login a documented stub since audible-activator
   uses the removed Selenium 3 API and the browser is intentionally not
   installed), `/job` decrypt (ffmpeg `-activation_bytes -c copy`, `-progress`
   parsed into a sibling `<out>.progress` file). Optional via empty
   `AUDIOBOOK_SIDECAR_URL`; in dev+prod compose + the release image matrix.
   Verified by building/running the container and exercising the full contract;
   decrypt fails cleanly with wrong bytes on a real `.aax`. A SUCCESSFUL decrypt
   still needs the user's real activation bytes.
3. (DONE) `internal/audible.Client` (Go side): mirrors `drm.Client`
   (`Health`/`Configured`/`Decrypt`) with audiobook-specific setup
   (`SetupBytes` / `SetupLogin`) and a package-level `Progress(path)` that reads
   the sidecar's progress file. 30-min timeout (a long decrypt). Tested against a
   mock AND the real running sidecar (contract verified end to end). Wiring it as
   a second optional client on the importer/server lands with the import step.
4. (DONE) `internal/audio`: `Read`/`CoverImage` for a clean M4B, parsing the
   MP4/ISO-BMFF boxes in **pure Go** (NOT ffprobe/ffmpeg: the Go side never
   shells out, ffmpeg lives only in the sidecar, so the distroless library image
   needs no new runtime dep). Reads `moov/mvhd` (duration), `moov/udta/meta/ilst`
   (iTunes tags: `©nam` title, `©ART` author, `aART` narrator, `©day` year,
   `covr` cover), and chapters from **either** representation:
   - **Nero `chpl`** box (a flat start-time + title list) is what ffmpeg's muxer
     writes; preferred when present.
   - **QuickTime chapter text track** (the audio trak's `tref/chap` points at a
     `text` trak whose `stts` + text samples give chapter times + titles) is what
     REAL Audible files carry: they have NO `chpl`. This was a gap caught by
     testing the parser against a real `.aax`; the synthetic ffmpeg fixture has a
     `chpl`, so it alone would not have exposed it. `Read` falls back to the text
     track when `chpl` is absent. The `.aax`'s `moov` is unencrypted, so the
     parser was validated directly against a real 27-chapter book (titles + start
     times match) before the clean-`.m4b` decrypt path even exists.
   Tests build a tiny chaptered M4B fixture in-process with ffmpeg (a TEST-only
   dep, skipped if ffmpeg is absent) and also force the text-track path by
   renaming the fixture's `chpl` box to `free`. Cover present/absent both
   covered. All checks green (vet + full suite).
5. (DONE) Schema/catalog: `format = "audio"`; `formatForPath`/`indexableExt`/
   `readMetadata`/`coverImageFor` branch on `.m4b` (in the library) vs `.aax`
   (not: decrypted at import). `readMetadata` maps `internal/audio`'s shared
   fields onto the upsert struct and also returns an `audioMeta{Narrator,
   Duration}` carried into two new, file-derived (never user-edited) columns
   `narrator` + `duration_secs` (migrated like `format` was, populated on every
   index, surfaced on `Book`). Gaps found in the code and closed in this step:
   - **Metadata embed:** `EmbedMetadata` would have fallen through to the epub
     writer for an audiobook and corrupted the `.m4b` (`internal/audio` is
     read-only, no writer). Now refused cleanly with a reason; an audio edit
     stays in the catalog only. Covered by a test.
   - **Download MIME:** the `/file` handler hard-coded epub/cbz; `.m4b` now
     downloads as `audio/mp4` with a `.m4b` name.
   - **Reader dispatch:** the `/read/{slug}` handler would have loaded an
     audiobook into the epub.js reader. It now returns 501 (player is step 8);
     the file is still downloadable. A visible placeholder, not silent breakage.
   Tested by indexing a real ffmpeg-built `.m4b` and asserting format=audio +
   narrator + duration. vet/golangci-lint clean, full suite green.
6. (DONE) Import: `importable()` + `sourceFor()` accept `.aax` (source
   `audible-import`). `pipeline()` routes `.aax` to a new `decryptAudible()`
   that, like the comic branch, skips the epub inspection and drives the
   audiobook sidecar. The sidecar's `/job` decrypt is synchronous, so
   `decryptAudible` polls the sibling `<out>.m4b.progress` file (via
   `audible.Progress`) in a goroutine and reports `onProgress("converting",
   frac, "NN%")` so the /imports bar fills during a multi-minute conversion;
   the clean `.m4b` then flows through the shared tail (verify -> Author/Title
   rename keeping `.m4b` -> index as `format=audio` -> archive the original
   `.aax` to done/). `verify()` parses the `.m4b` via `internal/audio`. The
   `audible.Client` is wired on the `Importer` (`Audible` field) and built in
   `main.go` from `AUDIOBOOK_SIDECAR_URL` / `-audiobook-sidecar`, independent of
   the ebook sidecar (the "one / other / both / none" surface): empty disables
   it and `.aax` is rejected into `failed/` with a clear reason. A startup
   health probe logs ready / needs-setup / down. Tested end to end with a mock
   sidecar that produces a real ffmpeg `.m4b` (asserts library path, format,
   narrator, archived original) and a disabled-sidecar reject test; the `.aax`
   unit cases are in `TestImportable`/`TestSourceFor`. A real `.aax` decrypt
   still needs the user's activation bytes (not yet extracted), the one path
   not exercisable here. vet/gofmt/golangci-lint clean, full suite green.
7. **Setup UI generalization (gap found in the code):** today `needsSetup` and the
   index form are singular and AdobeID-only. Generalize: the first-run page shows
   an independent **Ebook DRM** section (if the ebook sidecar is enabled +
   unconfigured) AND/OR an **Audiobook DRM** section (if the audiobook sidecar is
   enabled + unconfigured). Each form posts to its own setup endpoint; the
   audiobook form has a mode toggle (login vs paste-bytes). A disabled sidecar's
   section never renders. This is the "one / other / both / none" surface (§0).
8. Player: `audio.html` + `audio.js` + `/chapters`, format-dispatched at
   `/read/{slug}`; range-served file for seeking; position persisted. (Browser-
   verified by the user; JS checked statically, per convention.)
9. OPDS: emit the audio media type (decide on X4 inclusion).
10. `.aaxc` fast-follow: voucher-key path in the sidecar; reject voucherless
    `.aaxc` into `failed/`.

## 9. Risks / notes

- **Selenium fragility is the hard part**, exactly why it is quarantined in its
  own sidecar. The paste-bytes fallback means the feature is usable even if the
  Selenium retrieval breaks against an Audible site change.
- **Decryption is the safe part**: ffmpeg `-activation_bytes` is stable and
  lossless (stream copy), not a re-encode.
- **Activation bytes are an account secret**: stored in `/secrets` like the ADEPT
  key, written only at setup, never logged. The sidecar mounts `/secrets` and is
  the only component that reads them.
- **Long conversions**: bump the audiobook sidecar client timeout and confirm the
  job retention window covers multi-minute runs (see §5).
- **`.m4b` is large**: covers cache as usual (cheap), but the library will hold
  hundreds of MB per book; this is expected for audio and unchanged from how
  comics already stress disk.
- **Optional everywhere**: no audible sidecar -> `.aax` import is rejected
  clearly and the audiobook setup section is hidden, exactly like the existing
  no-DRM mode. Users who only want ebooks/comics are unaffected.
- **This unblocks audiobooks** but reuses the comic-era infrastructure (format
  discriminator, long-task jobs + SSE, optional-sidecar pattern) almost entirely;
  the genuinely new code is the sidecar, `internal/audio`, and the player.
