FROM public.ecr.aws/docker/library/golang:1.26-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends gcc libc6-dev && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=1 go build \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -trimpath \
    -o /apoci \
    ./cmd/apoci

FROM public.ecr.aws/docker/library/debian:bookworm-slim

ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=
LABEL org.opencontainers.image.title="apoci" \
      org.opencontainers.image.description="apoci — federated, self-hostable multi-format (OCI, npm, Cargo, PyPI) registry that publishes artifacts as an ActivityPub actor" \
      org.opencontainers.image.source="https://git.erwanleboucher.dev/eleboucher/apoci" \
      org.opencontainers.image.url="https://git.erwanleboucher.dev/eleboucher/apoci" \
      org.opencontainers.image.documentation="https://git.erwanleboucher.dev/eleboucher/apoci/src/branch/main/README.md" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.authors="Erwan Leboucher <erwanleboucher@gmail.com>" \
      org.opencontainers.image.vendor="Erwan Leboucher" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates wget && \
    rm -rf /var/lib/apt/lists/*

ENV PATH="/apoci:${PATH}" \
    APOCI_DATA_DIR="/apoci/storage"

USER 1000:1000

WORKDIR "/apoci/storage"
WORKDIR "/apoci/config"
WORKDIR "/apoci"

COPY --chown=1000:1000 --from=builder /apoci /apoci/apoci

VOLUME "/apoci/storage"
EXPOSE 5000

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget -q --spider http://localhost:5000/healthz || exit 1

ENTRYPOINT ["apoci"]
CMD ["serve", "-c", "/apoci/config/apoci.yaml"]
