# syntax=docker/dockerfile:1
FROM golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS builder
ARG TARGETARCH
RUN test "$TARGETARCH" = amd64
RUN apt-get update && apt-get install -y --no-install-recommends build-essential git ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git init . && git remote add origin https://github.com/router-for-me/CLIProxyAPI.git && git fetch --depth=1 origin 4a2c81864f31f39308e946c4c65e72147855da6e && git checkout --detach FETCH_HEAD && test "$(git rev-parse HEAD)" = 4a2c81864f31f39308e946c4c65e72147855da6e
COPY docs/patches/cpa-v8.0.2-scheduler-retry-after.patch /tmp/host.patch
RUN echo '86bb15f7a1c6d2629766cdf6983946dc0779e1e0cef5e9254c5bd195d3a1f52e  /tmp/host.patch' | sha256sum -c - && git apply --check /tmp/host.patch && git apply /tmp/host.patch
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w -X main.Version=8.0.2-retry-after -X main.Commit=30a9b6c28d39ad2fd5e662a947bad706e4726ba7 -X main.BuildDate=${BUILD_DATE}" -o /out/CLIProxyAPI ./cmd/server

FROM debian:bookworm@sha256:f37a335e82bca302e955fa39f9dfe28f1be618f016f8a2b56318e5a5111afc26 AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/* && mkdir /CLIProxyAPI
COPY --from=builder /out/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=builder /src/config.example.yaml /CLIProxyAPI/config.example.yaml
ARG REPO_REV=unknown
LABEL org.opencontainers.image.source="https://github.com/kurbezz/cpa-plugin-five-hour-quota-router" \
      org.opencontainers.image.revision="${REPO_REV}" \
      org.opencontainers.image.version="8.0.2-retry-after" \
      io.cpa.upstream.revision="4a2c81864f31f39308e946c4c65e72147855da6e" \
      io.cpa.patched.revision="30a9b6c28d39ad2fd5e662a947bad706e4726ba7" \
      io.cpa.patch.sha256="86bb15f7a1c6d2629766cdf6983946dc0779e1e0cef5e9254c5bd195d3a1f52e"
WORKDIR /CLIProxyAPI
EXPOSE 8317
ENV TZ=Asia/Shanghai
RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone
CMD ["./CLIProxyAPI"]

# Test tooling/plugin never enter the runtime target.
FROM builder AS test-plugin
WORKDIR /plugin
COPY go.mod go.sum ./
COPY *.go ./
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -buildmode=c-shared -ldflags="-s -w -X main.pluginVersion=0.5.1 -X main.usageEndpoint=http://127.0.0.1:19091/usage" -o /out/five-hour-quota-router.so .

FROM runtime AS verify
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY --from=test-plugin /out/five-hour-quota-router.so /test-plugin/five-hour-quota-router.so
COPY docker/tests/verify-image.py /verify-image.py
ENTRYPOINT ["python3", "/verify-image.py"]
CMD []
