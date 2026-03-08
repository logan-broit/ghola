# Build pg_recall extension for CNPG PostgreSQL 18
#
# Builder: compiles pg_recall from source using pgrx
# Final:   CNPG base image + pg_recall artifacts
#
# Build:  podman build --platform linux/amd64 -t ghcr.io/thinkwright/chapterhouse/cnpg-pg18:latest .
# Push:   podman push ghcr.io/thinkwright/chapterhouse/cnpg-pg18:latest

# ---------------------------------------------------------------------------
# Stage 1: Build pg_recall
# ---------------------------------------------------------------------------
FROM --platform=linux/amd64 rust:1.92-bookworm AS builder

# Install PostgreSQL 18 dev headers from PGDG
RUN apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates gnupg lsb-release \
    && echo "deb https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" \
        > /etc/apt/sources.list.d/pgdg.list \
    && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
        | gpg --dearmor -o /etc/apt/trusted.gpg.d/pgdg.gpg \
    && apt-get update && apt-get install -y --no-install-recommends \
        postgresql-server-dev-18 postgresql-18 \
        pkg-config libssl-dev libclang-dev clang \
    && rm -rf /var/lib/apt/lists/*

# Install cargo-pgrx matching our Cargo.toml pinned version
RUN cargo install cargo-pgrx --version "=0.17.0" --locked

# Initialize pgrx for pg18 (system-installed)
RUN cargo pgrx init --pg18 /usr/lib/postgresql/18/bin/pg_config

# Copy source
WORKDIR /build
COPY . .

# Build the extension package
RUN cargo pgrx package --pg-config /usr/lib/postgresql/18/bin/pg_config --features pg18

# ---------------------------------------------------------------------------
# Stage 2: CNPG base image + pg_recall
# ---------------------------------------------------------------------------
FROM --platform=linux/amd64 ghcr.io/cloudnative-pg/postgresql:18.1-system-trixie

# Copy pg_recall shared library
COPY --from=builder /build/target/release/pg_recall-pg18/usr/lib/postgresql/18/lib/pg_recall.so \
     /usr/lib/postgresql/18/lib/pg_recall.so

# Copy extension control and SQL files
COPY --from=builder /build/target/release/pg_recall-pg18/usr/share/postgresql/18/extension/pg_recall.control \
     /usr/share/postgresql/18/extension/pg_recall.control
COPY --from=builder /build/target/release/pg_recall-pg18/usr/share/postgresql/18/extension/pg_recall--0.4.0.sql \
     /usr/share/postgresql/18/extension/pg_recall--0.4.0.sql
