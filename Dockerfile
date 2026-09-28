FROM golang:1.25-alpine AS build

WORKDIR /src
ARG VERSION=dev
ARG COMMIT=unknown

COPY go.mod ./
RUN go mod download
COPY . .

RUN resolved_commit="${COMMIT}"; \
    if [ -z "${resolved_commit}" ] || [ "${resolved_commit}" = "unknown" ]; then \
      head_value="$(cat .git/HEAD 2>/dev/null || true)"; \
      case "${head_value}" in \
        "ref: "*) \
          ref="${head_value#ref: }"; \
          if [ -f ".git/${ref}" ]; then \
            resolved_commit="$(cat ".git/${ref}")"; \
          elif [ -f .git/packed-refs ]; then \
            resolved_commit="$(awk -v ref="${ref}" '$2 == ref { print $1; exit }' .git/packed-refs)"; \
          fi \
          ;; \
        ?*) resolved_commit="${head_value}" ;; \
      esac; \
    fi; \
    if [ -z "${resolved_commit}" ]; then resolved_commit="unknown"; fi; \
    ldflags="-s -w -X main.version=${VERSION} -X main.commit=${resolved_commit}"; \
    CGO_ENABLED=0 go build -trimpath -ldflags="${ldflags}" -o /out/gpt-codex-router ./cmd/gpt-codex-router

FROM alpine:3.22 AS codex-runtime
ARG CODEX_VERSION=0.157.1
ARG TARGETARCH
RUN apk add --no-cache ca-certificates curl tar \
    && case "${TARGETARCH}" in \
         amd64) triple='x86_64-unknown-linux-musl'; expected='e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2' ;; \
         arm64) triple='aarch64-unknown-linux-musl'; expected='4c6b1c17c1c5fd0d4fb2951b7481867b95ea732b1feab269c98588b15db16253' ;; \
         *) echo "unsupported Docker architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && archive="codex-${triple}.tar.gz" \
    && curl -fsSL --retry 3 -o "/tmp/${archive}" "https://github.com/openai/codex/releases/download/rust-v${CODEX_VERSION}/${archive}" \
    && echo "${expected}  /tmp/${archive}" | sha256sum -c - \
    && mkdir -p /out \
    && tar -xzf "/tmp/${archive}" -C /out \
    && mv "/out/codex-${triple}" /out/codex \
    && chmod 0755 /out/codex \
    && /out/codex --version

FROM alpine:3.22
ARG CODEX_VERSION=0.157.1

LABEL org.opencontainers.image.title="GPT Codex Router" \
      org.opencontainers.image.description="Local ChatGPT subscription profile router for Codex" \
      org.opencontainers.image.source="https://github.com/munlucky/codex-account-pool" \
      org.opencontainers.image.licenses="MIT"

RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 gptcodexrouter \
    && adduser -S -D -H -u 10001 -G gptcodexrouter gptcodexrouter \
    && mkdir -p /data \
    && chown 10001:10001 /data

COPY --from=build /out/gpt-codex-router /usr/local/bin/gpt-codex-router
COPY --from=codex-runtime /out/codex /usr/local/bin/codex

ENV GPT_CODEX_ROUTER_HOME=/data \
    GPT_CODEX_ROUTER_CONTAINER=1 \
    GPT_CODEX_ROUTER_CODEX_CLIENT_VERSION=${CODEX_VERSION}

EXPOSE 8317
USER 10001:10001

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8317/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/gpt-codex-router"]
CMD ["serve", "--listen", "0.0.0.0:8317"]
