FROM public.ecr.aws/docker/library/debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    PATH="/opt/venv/bin:/root/.cargo/bin:/opt/dotnet:/usr/local/go/bin:${PATH}" \
    CARGO_HOME=/root/.cargo \
    RUSTUP_HOME=/root/.rustup \
    DOTNET_CLI_TELEMETRY_OPTOUT=1 \
    DOTNET_NOLOGO=1 \
    GOTOOLCHAIN=local

# Base tools + node/npm + python (libicu is required by the .NET runtime).
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates git nodejs npm python3 python3-venv libicu72 \
    && rm -rf /var/lib/apt/lists/*

# Python build + twine in an isolated venv. setuptools/wheel are preinstalled so
# `python -m build --no-isolation` needs no network at test time.
RUN python3 -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-cache-dir --upgrade pip build twine setuptools wheel

# Rust toolchain — modern cargo with sparse-registry support.
RUN curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable

# .NET SDK (for dotnet pack / dotnet nuget push). The install script needs bash
# (it uses `set -o pipefail`), so don't run it with dash/sh.
RUN curl -fsSL https://dot.net/v1/dotnet-install.sh -o /tmp/dotnet-install.sh \
    && bash /tmp/dotnet-install.sh --channel 8.0 --install-dir /opt/dotnet --no-path \
    && rm /tmp/dotnet-install.sh

# Go toolchain (for `go get` against the goproxy backend). Arch-aware download.
RUN GO_VERSION=1.23.4 \
    && case "$(dpkg --print-architecture)" in \
         amd64) GOARCH=amd64 ;; arm64) GOARCH=arm64 ;; *) echo "unsupported arch" && exit 1 ;; \
       esac \
    && curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" -o /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz

COPY federation.sh /tests/federation.sh
COPY packages.sh /tests/packages.sh
COPY run.sh /tests/run.sh
ENTRYPOINT ["sh", "/tests/run.sh"]
