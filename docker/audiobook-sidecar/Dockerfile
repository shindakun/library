# Audiobook DRM sidecar: the quarantined ffmpeg corner. Removes Audible DRM from
# a .aax by decoding the AAX-encrypted audio with the account's activation bytes,
# producing a clean, chaptered .m4b. Stdlib-only Python (no pip deps) + ffmpeg.
#
# v1 ships the PASTE-activation-bytes setup path (reliable). The Selenium login
# retrieval is stubbed (audible-activator uses the removed Selenium 3 API and
# breaks against Audible changes); the browser + Selenium are intentionally NOT
# installed so the image stays small. When the login path is hardened, add
#   chromium chromium-driver + pip install selenium
# here and implement setup_login in worker.py.
FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV SECRETS_DIR=/secrets \
    WORK_DIR=/work \
    PORT=7100

COPY worker.py /opt/worker.py

EXPOSE 7100
CMD ["python", "/opt/worker.py"]
