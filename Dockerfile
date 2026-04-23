FROM golang:1.26-alpine AS builder

WORKDIR /app
COPY go.mod go.sum *.go ./
RUN go mod download && \
    CGO_ENABLED=0 GOOS=linux go build -o db-sync .

FROM ubuntu:24.04

LABEL org.opencontainers.image.title="govuk-db-sync"
LABEL org.opencontainers.image.authors="GOV.UK Platform Engineering"
LABEL org.opencontainers.image.description="DB Sync tool for GOV.UK databases"
LABEL org.opencontainers.image.source="https://github.com/alphagov/govuk-db-sync"
LABEL org.opencontainers.image.vendor="GDS"

# Prevent interactive prompts during apt-get installations
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    wget \
    ca-certificates \
    lsb-release && \
    # Workaround for DocumentDB 3.6 support
    # TODO: Remove me when DocumentDB 3.6 support is no longer needed
    GOARCH=$(dpkg --print-architecture) && \
    if [ "$GOARCH" = "amd64" ]; then \
        wget -qO /tmp/libssl.deb http://archive.ubuntu.com/ubuntu/pool/main/o/openssl/libssl1.1_1.1.1f-1ubuntu2_amd64.deb; \
    else \
        wget -qO /tmp/libssl.deb http://ports.ubuntu.com/ubuntu-ports/pool/main/o/openssl/libssl1.1_1.1.1f-1ubuntu2_arm64.deb; \
    fi && \
    dpkg -i /tmp/libssl.deb && rm /tmp/libssl.deb && \
    mkdir -p /etc/apt/keyrings && \
    # Download and save MyDumper, Postgres and MongoDB 4.4 GPG keys
    wget -O- 'https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x1D357EA7D10C9320371BDD0279EA15C0E82E34BA&exact=on' > /etc/apt/keyrings/mydumper.asc && \
    wget -O- 'https://www.postgresql.org/media/keys/ACCC4CF8.asc' > /etc/apt/keyrings/pgdg.asc && \
    # TODO: Replace me with the correct MongoDB PGP Key for Ubuntu 24.04 when DocDB 3.6 is retired...
    wget -O- 'https://www.mongodb.org/static/pgp/server-4.4.asc' > /etc/apt/keyrings/mongodb.asc && \
    # Add MyDumper, Postgres and MongoDB Ubuntu Repos...
    echo "deb [signed-by=/etc/apt/keyrings/mydumper.asc] https://mydumper.github.io/mydumper/repo/apt/ubuntu $(lsb_release -cs) main" > /etc/apt/sources.list.d/mydumper.list && \
    echo "deb [signed-by=/etc/apt/keyrings/pgdg.asc] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list && \
    # TODO: Replace me with the correct MongoDB repo for Ubuntu 24.04 when DocDB 3.6 is retired...
    echo "deb [arch=amd64,arm64 signed-by=/etc/apt/keyrings/mongodb.asc] https://repo.mongodb.org/apt/ubuntu focal/mongodb-org/4.4 multiverse" > /etc/apt/sources.list.d/mongodb.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends mysql-client postgresql-client mydumper mongodb-org-tools mongodb-org-shell zstd && \
    apt-get remove -y wget && \
    apt-get autoremove -y && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy the compiled bins from builder stage
COPY --from=builder /app/db-sync /usr/local/bin/db-sync

# Create directory for transformation scripts
RUN mkdir -p /scripts

ENTRYPOINT [ "db-sync" ]
