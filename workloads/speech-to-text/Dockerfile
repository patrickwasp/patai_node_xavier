# Speech to text for the Xavier: Cactus's engine for Whistle on the CPU, whisper.cpp for Whisper with CUDA 11.4 and
# sm_72, patai-transcribe in front, and an FFmpeg of its own that only turns a recording into WAV. Built in NVIDIA's
# CUDA image, run on the shared CUDA base (workloads/cuda-base), so its own layer is only its programs.
# The shared CUDA base, by digest; scripts/publish-workloads sets it. A node that built the base can pass its own.
ARG BASE=ghcr.io/patrickwasp/patai-cuda-base@sha256:ce9e04549c72be4abf6fae65a8edf0b4e59130ed0e4f8fe6d447b031fee87e99
FROM nvcr.io/nvidia/l4t-cuda:11.4.19-devel@sha256:cff51f4845ff298faa3e41e42f1f19d037bafdb354af5fcf04c93fe981317d02 AS tools

RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential ca-certificates curl git python3-pip xz-utils \
 && pip3 install 'cmake<4' \
 && rm -rf /var/lib/apt/lists/*

FROM tools AS whisper
ARG WHISPER_VERSION=v1.9.5
RUN git clone --depth 1 -b "$WHISPER_VERSION" https://github.com/ggml-org/whisper.cpp /opt/whisper.cpp
WORKDIR /opt/whisper.cpp
# Carmel is ARMv8.2 with FP16 and no dotprod, as for llama.cpp.
RUN cmake -B build -DCMAKE_BUILD_TYPE=Release \
      -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=72 \
      -DGGML_NATIVE=OFF -DGGML_CPU_ARM_ARCH=armv8.2-a+fp16 \
      -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DWHISPER_BUILD_SERVER=ON \
 && cmake --build build -j"$(nproc)" --target whisper-server whisper-cli whisper-bench \
 && mkdir -p /out/bin /out/lib \
 && cp build/bin/whisper-server build/bin/whisper-cli build/bin/whisper-bench /out/bin/ \
 && find build -name '*.so*' -exec cp -P {} /out/lib/ \; \
 && strip --strip-unneeded /out/bin/*

# The server hands each recording to `ffmpeg` for WAV at 16 kHz. Browsers record Opus in WebM (Chrome) or Ogg
# (Firefox), and AAC in MP4 (Safari), and Open WebUI's slim image passes them on as they are. Only those, MP3, FLAC and
# WAV: a few MB, LGPL, against Ubuntu's 200 MB with every codec.
FROM tools AS ffmpeg
ARG FFMPEG_VERSION=9.0.2
ARG FFMPEG_SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
RUN curl -fsSL -o /ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" \
 && echo "${FFMPEG_SHA256}  /ffmpeg.tar.xz" | sha256sum -c - \
 && mkdir /opt/ffmpeg && tar -xJf /ffmpeg.tar.xz -C /opt/ffmpeg --strip-components=1
WORKDIR /opt/ffmpeg
RUN ./configure --disable-everything --disable-autodetect --disable-network --disable-doc --disable-debug \
      --disable-ffplay --disable-ffprobe --disable-avdevice --disable-swscale \
      --enable-protocol=file \
      --enable-demuxer=matroska,ogg,mov,wav,mp3,flac,aac \
      --enable-parser=opus,vorbis,aac,mpegaudio,flac \
      --enable-decoder=opus,vorbis,aac,mp3,flac,pcm_s16le,pcm_s24le,pcm_s32le,pcm_f32le,pcm_u8,pcm_alaw,pcm_mulaw \
      --enable-filter=abuffer,abuffersink,aformat,anull,aresample \
      --enable-encoder=pcm_s16le --enable-muxer=wav \
 && make -j"$(nproc)" ffmpeg \
 && mkdir -p /out/bin && cp ffmpeg /out/bin/ && strip --strip-unneeded /out/bin/ffmpeg

# Cactus's engine as Cactus builds it for Linux on ARM64, from source commit 89a1c50 of github.com/cactus-compute/cactus,
# checked against its release's manifest. It needs only glibc 2.29.
FROM tools AS cactus
ARG CACTUS_VERSION=4.1.0
ARG CACTUS_SHA256=088137b428c1be5cacb022a78a56f82815e5d60767002874afcc2ca7cf446074
RUN curl -fsSL -o /cactus \
      "https://huggingface.co/Cactus-Compute/cactus/resolve/569a10bb4f0e87331abcf874903c0e5139d38aae/releases/${CACTUS_VERSION}/linux-arm64/cactus" \
 && echo "${CACTUS_SHA256}  /cactus" | sha256sum -c - \
 && chmod 755 /cactus

FROM golang:1.26-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS server
WORKDIR /src
COPY patai-transcribe/ .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/patai-transcribe .

FROM ${BASE}
COPY --from=whisper /out/bin/ /usr/local/bin/
COPY --from=whisper /out/lib/ /usr/local/lib/
COPY --from=ffmpeg /out/bin/ /usr/local/bin/
COPY --from=cactus /cactus /usr/local/bin/cactus
COPY --from=server /out/patai-transcribe /usr/local/bin/patai-transcribe
RUN ldconfig
WORKDIR /models
ENTRYPOINT ["/usr/local/bin/patai-transcribe"]
LABEL org.opencontainers.image.source=https://github.com/patrickwasp/patai_node_xavier
