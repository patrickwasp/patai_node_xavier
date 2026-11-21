# The console (docs/console.md): the UI built with Node and embedded in static Go binaries for the
# unit. There's no Node or Go on a unit. `docker run` prints the install tree as a tar, for
# scripts/build-product, as docker/rauc.Dockerfile does. Images are pinned; go.sum and pnpm-lock.yaml
# pin the rest.
FROM node:24-alpine@sha256:8a32c8cf649a49915a66e81e7a609223c708d0adabf288ee3f4077d765542f85 AS ui
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm install -g pnpm@12.16.1
COPY console/ui/ /ui/
WORKDIR /ui
RUN pnpm install --frozen-lockfile && pnpm build

FROM golang:1.26-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308
COPY console/ /src/
# The recipes and apps, for the test that their manifests match them.
COPY workloads/ /workloads/
COPY apps/ /apps/
COPY --from=ui /ui/out/ /src/internal/web/dist/
WORKDIR /src
ENV CGO_ENABLED=0
RUN go test ./... \
 && GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o /out/usr/local/sbin/ ./cmd/... \
 && install -D -m 644 STATE_FORMAT /out/usr/share/patai/state-format \
 && go run ./cmd/patai-formats declared /workloads > /out/usr/share/patai/data-formats.json
CMD ["tar", "-C", "/out", "-cf", "-", "."]
