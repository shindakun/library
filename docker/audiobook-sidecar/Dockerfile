# Audiobook DRM sidecar: the quarantined ffmpeg corner. Removes Audible DRM from
# a .aax / .aaxc, producing a clean, chaptered .m4b.
#
# Two setup paths put the account activation bytes in /secrets:
#   - paste: the user pastes their 8 hex bytes (always reliable).
#   - login: the maintained `audible` library authenticates with the Amazon
#     email/password and fetches the bytes over Audible's API. This is pure
#     HTTP + crypto, NO browser/Selenium, so the image stays small. The library
#     is lazy-IMPORTED inside setup_login (worker.py), so its import cost is paid
#     only when a login is attempted and a dep issue can't break decrypt/health.
FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# `audible` (mkb79) drives the no-browser login -> activation-bytes flow.
# Pinned to the latest PyPI release; it exposes Authenticator.from_login and
# get_activation_bytes, which setup_login uses.
RUN pip install --no-cache-dir "audible==0.8.2"

ENV SECRETS_DIR=/secrets \
    WORK_DIR=/work \
    PORT=7100

COPY worker.py /opt/worker.py

EXPOSE 7100
CMD ["python", "/opt/worker.py"]
