# Piper and sanoTTS for the text-to-speech APIs (docs/apps.md, Voice): Piper's Python package and ONNX Runtime for the
# CPU, sanoTTS's NumPy runtime from its source at a pinned commit, espeak-ng through phonemizer-fork for both, py3langid
# to find a text's language, and patai-speech, OpenAI's /v1/audio/speech in front of them. Piper, espeak-ng,
# phonemizer-fork and sanoTTS's voices are GPL-3.0: their source is in this repository's `voices-sources` release
# (docs/apps.md, Voice).
FROM python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
ARG SANOTTS_COMMIT=55c6f39b2b535b5a591667a0852a6c2778320fab
# Every package as the published image has them, and the ones they bring, so a build brings no other: ONNX Runtime's
# telemetry above all, which a later release could start differently.
RUN pip install --no-cache-dir \
      attrs==26.1.0 babel==2.18.0 cloudpickle==3.1.3 csvw==4.2.0 dlinfo==2.0.0 espeakng-loader==0.2.4 \
      flatbuffers==25.12.19 isodate==0.7.2 joblib==1.6.0 jsonschema-specifications==2025.9.1 jsonschema==4.26.0 \
      language-tags==1.3.2 numpy==2.5.4 onnxruntime==1.32.0 packaging==26.3 pathvalidate==3.3.1 \
      phonemizer-fork==3.3.2 piper-tts==1.8.0 protobuf==7.36.2 py3langid==0.4.0 pyparsing==3.3.3 \
      python-dateutil==4.0.0 rdflib==7.6.0 referencing==0.37.0 regex==2026.11.19 rfc3986==1.5.0 rpds-py==2026.9.1 \
      segments==2.4.0 termcolor==3.3.0 typing_extensions==4.16.0 tzdata==2026.6 uritemplate==4.2.0 \
      "sanotts @ https://github.com/Ampixa/sanoTTS/archive/${SANOTTS_COMMIT}.tar.gz#subdirectory=pypkg" \
 && pip uninstall -y pip setuptools \
 && cd /usr/local/lib/python3.12/site-packages \
 && rm -rf piper/hebrew onnxruntime/transformers onnxruntime/tools \
 && find babel/locale-data -type f ! -name root.dat ! -name en.dat ! -name 'en_*.dat' -delete \
 && find /usr/local/lib/python3.12 -type d -name __pycache__ -exec rm -rf {} +
# ONNX Runtime sends Microsoft telemetry from Linux too, unless told before it starts: it did on patai-dev, 2026-11-15.
# sanoTTS fetches a voice it hasn't got from Hugging Face: every voice is given it from /models, and this says where
# its cache would be, read-only.
ENV ORT_DISABLE_TELEMETRY=1 SANOTTS_VOICE_SOURCE=hf XDG_CACHE_HOME=/tmp
COPY patai-speech /usr/local/bin/patai-speech
# As root, which owns the voices patai-fetch keeps: the recipe takes every capability away.
WORKDIR /models
EXPOSE 8080
ENTRYPOINT ["python3", "-I", "-B", "/usr/local/bin/patai-speech"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
