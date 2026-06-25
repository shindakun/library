# Ebook DRM sidecar: the quarantined Python corner. Runs the standalone
# acsm-calibre-plugin scripts (fulfillment) + DeDRM's ineptepub (decryption).
# No Calibre, no GUI, just lxml + pycryptodome (see docs/DESIGN.md §2.4 Strategy B).
FROM python:3.12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates wget unzip libxml2 libxslt1.1 libssl3 \
    && rm -rf /var/lib/apt/lists/*

# pycryptodome (libadobe imports Cryptodome/Crypto), rsa, lxml.
RUN pip install --no-cache-dir lxml pycryptodome rsa

# acsm-calibre-plugin: fulfillment (libadobe* + fulfill.py / register / get_key).
RUN git clone --depth 1 https://github.com/Leseratte10/acsm-calibre-plugin.git /opt/src-acsm \
    && cp -r /opt/src-acsm/calibre-plugin /opt/acsm

# libadobe.py REQUIRES oscrypto (for Adobe pkcs12 parsing) + its dep asn1crypto.
# Upstream oscrypto 1.3.0 is broken on OpenSSL 3, so the plugin ships an
# author-validated fork. We fetch the EXACT bundles package_modules.sh uses
# (see acsm repo: oscrypto fork dated 2023-12-19, asn1crypto 1.5.1) and place
# them on the module path, mirroring how the Calibre plugin loads them.
RUN cd /opt/acsm \
    && wget -q https://github.com/Leseratte10/acsm-calibre-plugin/releases/download/config/asn1crypto_1.5.1.zip -O asn1crypto.zip \
    && wget -q https://github.com/Leseratte10/acsm-calibre-plugin/releases/download/config/oscrypto_1.3.0_fork_2023-12-19.zip -O oscrypto.zip \
    && unzip -q asn1crypto.zip -d /opt/acsm/_deps \
    && unzip -q oscrypto.zip -d /opt/acsm/_deps \
    && rm asn1crypto.zip oscrypto.zip

# DeDRM_tools: ADEPT epub decryption. Kept as a package dir so the relative
# imports in ineptepub.py work via `python -m DeDRM_plugin.ineptepub`.
RUN git clone --depth 1 https://github.com/noDRM/DeDRM_tools.git /opt/src-dedrm \
    && cp -r /opt/src-dedrm/DeDRM_plugin /opt/DeDRM_plugin

# oscrypto/asn1crypto packages live one level inside their extracted dirs; put
# the containing dirs on PYTHONPATH so `import oscrypto` / `import asn1crypto`
# resolve to the OpenSSL-3-capable fork.
ENV ACSM_TOOLS_DIR=/opt/acsm \
    ACSM_DEPS=/opt/acsm/_deps/oscrypto:/opt/acsm/_deps/asn1crypto \
    DEDRM_PARENT=/opt \
    SECRETS_DIR=/secrets \
    WORK_DIR=/work \
    PORT=7000

COPY worker.py /opt/worker.py
COPY setup.py /opt/setup.py

EXPOSE 7000
CMD ["python", "/opt/worker.py"]
