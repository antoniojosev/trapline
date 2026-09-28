# The image is the binary, and nothing else.
#
# This is the one place where the CGO-free choice (ADR 009) pays a visible
# dividend: a static binary needs no base image, so `FROM scratch` is not a
# stunt, it is the honest description of what has to ship. What that buys is
# not size — it is that the published claim "one binary, no dependencies" stays
# true in the container too, and that this image has no distribution to patch,
# no shell to land in, and no package manager to find a CVE in.
#
#   docker build -t trapline .
#   docker run -p 9000:9000 -v trapline-data:/data trapline
#
# What scratch costs, stated rather than discovered:
#   - No shell, so `docker exec` and `docker run --entrypoint sh` do not work.
#     The CLI is the interface: `docker exec <c> /trapline issues list`.
#   - No tzdata. Everything stored and reported is UTC, so nothing asks for a
#     zone; if that ever changes, the fix is importing time/tzdata, not a base
#     image.
#   - /tmp has to be created here, because SQLite spills there.

ARG GO_VERSION=1.26.6

FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

# Dependencies first: they change on their own schedule, and separating them
# keeps a one-line source edit from re-downloading the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamped the same way goreleaser stamps a release, so `trapline version`
# inside a container is as meaningful as it is outside one.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X github.com/antoniojosev/trapline/internal/adapters/cli.Version=${VERSION}" \
      -o /out/trapline ./cmd/trapline

# The scratch stage cannot run commands, so everything it needs is built here:
# an empty state directory and a /tmp, both owned by the unprivileged user the
# image runs as. /data has to be owned before it becomes a VOLUME, because a
# named volume created from this image inherits these permissions and there is
# no later moment to fix them from inside a container with no shell.
RUN mkdir -p /out/data /out/tmp \
 && chown 65532:65532 /out/data /out/tmp \
 && chmod 1777 /out/tmp \
 && printf 'trapline:x:65532:65532:trapline:/:/sbin/nologin\n' > /out/passwd \
 && printf 'trapline:x:65532:\n' > /out/group


FROM scratch

# Outbound TLS is a feature, not an accident: alert webhooks and uptime checks
# verify certificates, and on scratch there is no trust store to
# verify against unless one is put here.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/passwd /etc/passwd
COPY --from=build /out/group /etc/group
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/tmp /tmp
COPY --from=build /out/trapline /trapline

# Numeric, because a scratch image has no name resolution beyond the passwd
# file above and numbers are what the kernel and any `--user` override speak.
USER 65532:65532

VOLUME /data
EXPOSE 9000

ENV TRAPLINE_DB=/data/trapline.db \
    TRAPLINE_ADDR=0.0.0.0:9000

# The listen address deliberately differs from the binary's own default of
# loopback. Outside a container that default is a safety property: a fresh
# install must not become publicly reachable by accident. Inside one it would
# only mean "unreachable", because the network namespace already provides the
# isolation and nothing is exposed until the operator publishes a port.

# TRAPLINE_ORIGIN has no default on purpose. It is the public address SDKs
# send events to, it cannot be inferred from inside a container, and guessing
# it would hand out DSNs that resolve nowhere. The server says so at startup.

# The healthcheck asks the narrow question a supervisor can act on: is the
# state on disk readable and shaped the way this build expects. It makes no
# HTTP request — an unreachable API is usually a proxy or a port publish, and
# restarting the container would not fix either — and it creates nothing, so a
# broken volume mount reports unhealthy instead of quietly starting over on a
# fresh empty database.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --start-interval=1s --retries=3 \
  CMD ["/trapline", "doctor", "-db", "/data/trapline.db", "--quick"]

ENTRYPOINT ["/trapline"]
CMD ["serve"]
