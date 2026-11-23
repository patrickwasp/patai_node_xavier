# Kokoro and Piper for the text-to-speech APIs (docs/apps.md, Voice): ONNX Runtime for the CPU, misaki's phonemes
# for Kokoro (its English lexicon with spaCy's small English model, and espeak-ng for the rest), Piper's Python
# package, which brings its own espeak-ng, for the Piper voices, py3langid to find a text's language, and
# patai-speech, OpenAI's /v1/audio/speech in front of them. No torch: misaki's transformer extra isn't installed.
FROM python:3.12.16-slim@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b
# Every package as the published image has them, and the ones they bring, so a build brings no other: ONNX Runtime's
# telemetry above all, which a later release could start differently. spaCy's English model is checked by its hash:
# misaki downloads it if it isn't installed.
RUN pip install --no-cache-dir \
      addict==2.4.0 annotated-doc==0.0.7 annotated-types==0.8.0 anyio==4.16.0 attrs==26.1.0 babel==2.18.0 \
      blis==1.3.4 catalogue==2.0.10 certifi==2026.11.20 charset-normalizer==3.5.3 click==8.5.1 \
      cloudpathlib==0.26.0 cloudpickle==3.1.3 confection==1.3.4 csvw==4.2.0 cymem==2.0.16 dlinfo==2.0.0 \
      docopt==0.6.2 espeakng-loader==0.2.4 flatbuffers==25.12.19 h11==0.16.0 httpcore==1.0.9 httpx==0.28.1 \
      idna==3.20 isodate==0.7.2 Jinja2==3.1.6 joblib==1.6.0 jsonschema-specifications==2025.9.1 jsonschema==4.26.0 \
      language-tags==1.3.2 markdown-it-py==4.3.0 MarkupSafe==3.0.4 mdurl==0.1.2 misaki==0.9.4 murmurhash==1.0.16 \
      num2words==0.5.14 numpy==2.5.4 onnxruntime==1.32.0 packaging==26.3 pathvalidate==3.3.1 \
      phonemizer-fork==3.3.2 piper-tts==1.8.0 preshed==3.0.15 protobuf==7.36.2 py3langid==0.4.0 pydantic==2.14.4 \
      pydantic_core==2.50.5 Pygments==2.21.0 pyparsing==3.3.3 python-dateutil==4.0.0 rdflib==7.6.0 \
      referencing==0.37.0 regex==2026.11.19 requests==2.34.2 rfc3986==1.5.0 rich==15.0.0 rpds-py==2026.9.1 \
      segments==2.4.0 shellingham==1.5.4 smart_open==10.0.0 spacy-legacy==3.0.12 spacy-loggers==1.0.5 \
      spacy==3.8.18 srsly==2.5.6 termcolor==3.3.0 thinc==8.3.14 tqdm==4.70.1 typer==0.29.2 \
      typing-inspection==0.4.5 typing_extensions==4.16.0 tzdata==2026.6 uritemplate==4.2.0 urllib3==2.8.0 \
      wasabi==1.1.3 weasel==1.0.1 wrapt==2.5.2 \
      "en_core_web_sm @ https://github.com/explosion/spacy-models/releases/download/en_core_web_sm-3.8.0/en_core_web_sm-3.8.0-py3-none-any.whl#sha256=1932429db727d4bff3deed6b34cfc05df17794f4a52eeb26cf8928f7c1a0fb85" \
 && python3 -c 'import spacy; assert spacy.util.is_package("en_core_web_sm")' \
 && pip uninstall -y pip setuptools \
 && cd /usr/local/lib/python3.12/site-packages \
 && rm -rf piper/hebrew onnxruntime/transformers onnxruntime/tools \
 && find babel/locale-data -type f ! -name root.dat ! -name en.dat ! -name 'en_*.dat' -delete \
 && find /usr/local/lib/python3.12 -type d -name __pycache__ -exec rm -rf {} +
# ONNX Runtime sends Microsoft telemetry from Linux too, unless told before it starts: it did on patai-dev, 2026-11-15.
ENV ORT_DISABLE_TELEMETRY=1
COPY patai-speech /usr/local/bin/patai-speech
# As root, which owns the voices patai-fetch keeps: the recipe takes every capability away.
WORKDIR /models
EXPOSE 8080
ENTRYPOINT ["python3", "-I", "-B", "/usr/local/bin/patai-speech"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
