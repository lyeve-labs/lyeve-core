# syntax=docker/dockerfile:1
#
# Runtime image for the LyEve CMS engine: the `lyeve` server with every plugin
# compiled in, plus the SQL migrations.
#
# bin/lyeve is built outside this Dockerfile. The release builds it from
# cmd/lyeve, which compiles every plugin in through a Go workspace. A clone of
# this repository builds the engine alone into the same path:
#
#   GOWORK=off CGO_ENABLED=0 go build -trimpath -o bin/lyeve ./cmd/lyeve-core
#   docker build -t lyeve-core:dev .
#
# The binary is static (CGO off, libwebp embedded as WASM), so the base is
# distroless/static-debian12: CA certs, tzdata, a nonroot user. No libc, shell,
# or package manager. No shell means the HEALTHCHECK runs the binary's own
# `healthcheck` subcommand.

# Stage 1 prepares /app with the right ownership (distroless has no shell to
# mkdir/chown). Pinned to $BUILDPLATFORM so cross-arch builds need no QEMU: it
# only arranges prebuilt files, and the final stage only COPYs them.
FROM --platform=$BUILDPLATFORM debian:bookworm-slim@sha256:7b140f374b289a7c2befc338f42ebe6441b7ea838a042bbd5acbfca6ec875818 AS rootfs
COPY bin/lyeve /app/lyeve
COPY migrations /app/migrations
# 65532 = distroless's nonroot user. Pre-create uploads: the health probe stats
# it, and a mounted volume inherits this ownership so the process can write.
RUN install -d /app/uploads && chown -R 65532:65532 /app
# /var/lib/lyeve holds the token signing keypair (JWT_KEY_PATH) and any state
# the licensing implementation writes. distroless ships /var/lib root-owned
# 0755, so uid 65532 cannot create the directory at boot, and there is no
# shell to do it first. That is fatal rather than cosmetic: APP_ENV
# defaults to production, where a signing key the server cannot write aborts
# the boot instead of degrading to HS256. Staged under /state because the final
# image can only COPY.
RUN install -d -m 0700 -o 65532 -g 65532 /state/lyeve

FROM gcr.io/distroless/static-debian12@sha256:a9fcaedd4c9b59e12dd65d954f0b5044f19b0647a8a3712e77205df9e7b102cd
COPY --from=rootfs --chown=65532:65532 /app /app
# --chmod is explicit because COPY does not carry the staged 0700 over. It
# would land 0755 and leave the signing key's directory world-listable. A
# volume mounted here inherits this ownership, as with /app/uploads.
COPY --from=rootfs --chown=65532:65532 --chmod=0700 /state/lyeve /var/lib/lyeve
# The MPL-2.0 and other notices must travel with the binary they cover.
COPY LICENSE THIRD_PARTY_NOTICES.md /usr/share/doc/lyeve/
WORKDIR /app

ENV MIGRATIONS_PATH=/app/migrations \
    STORAGE_LOCAL_PATH=/app/uploads \
    ADMIN_LISTEN_ADDR=0.0.0.0:3001 \
    API_LISTEN_ADDR=0.0.0.0:3002

# 3001 = admin server, 3002 = public API. 3003 and 3004 are reserved for
# plugins that open their own listener. Exposing them commits to nothing until
# a deployment maps them.
EXPOSE 3001 3002 3003 3004

USER 65532:65532

# No shell or wget in distroless, so the binary probes its own /healthz.
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/app/lyeve", "healthcheck"]

ENTRYPOINT ["/app/lyeve"]
