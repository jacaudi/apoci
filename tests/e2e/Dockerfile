FROM public.ecr.aws/docker/library/debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends curl ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY federation.sh /tests/federation.sh
ENTRYPOINT ["sh", "/tests/federation.sh"]
