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
    CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${resolved_commit}" \
      -o /out/gpt-codex-router \
      ./cmd/gpt-codex-router

FROM alpine:3.22

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

ENV GPT_CODEX_ROUTER_HOME=/data \
    GPT_CODEX_ROUTER_CONTAINER=1

EXPOSE 8317
USER 10001:10001

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -q -O - http://127.0.0.1:8317/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/gpt-codex-router"]
CMD ["serve", "--listen", "0.0.0.0:8317"]
