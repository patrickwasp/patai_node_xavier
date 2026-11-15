# Piper for the text-to-speech APIs (docs/apps.md, Voice): its Python package, which brings espeak-ng and ONNX Runtime
# for the CPU, and patai-speech, OpenAI's /v1/audio/speech in front of it. Its own HTTP server isn't used: it downloads
# voices when asked.
FROM python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
ARG PIPER_VERSION=1.8.0
RUN pip install --no-cache-dir "piper-tts==${PIPER_VERSION}" \
 && pip uninstall -y pip setuptools \
 && find /usr/local/lib/python3.12 -type d -name __pycache__ -exec rm -rf {} +
# ONNX Runtime sends Microsoft telemetry from Linux too, unless told before it starts: it did on patai-dev, 2026-11-15.
ENV ORT_DISABLE_TELEMETRY=1
COPY patai-speech /usr/local/bin/patai-speech
# As root, which owns the voices patai-fetch keeps: the recipe takes every capability away.
WORKDIR /models
EXPOSE 8080
ENTRYPOINT ["python3", "-I", "-B", "/usr/local/bin/patai-speech"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
