# Deploying trapline

One binary and one file of state. That is the whole operational model, and
everything below is a variation on where to put the two.

Both routes are tested rather than described: `./scripts/install-test.sh`
builds a real release and installs it on a clean Debian, and the nightly
workflow starts the container image and waits for its healthcheck.

## Which one

**systemd** if the machine is yours and you already run services on it. There
is no daemon between you and the process, `journalctl` works the way you
expect, and upgrading is replacing one file.

**Docker** if the machine already runs containers, or if you would rather not
think about a Go binary's user and state directory. The image is `FROM
scratch`, so it is the binary with a certificate bundle — there is no
distribution inside it to keep patched.

Neither is more supported than the other.

## The one setting that must be set

`TRAPLINE_ORIGIN` — the public address SDKs will send events to, for example
`https://errors.example.com`.

It cannot be inferred. Behind a proxy, the request arrives claiming to be from
loopback while the DSN handed out in the panel has to say the real domain. Get
it wrong and every DSN the panel shows works on that machine and nowhere else,
which looks exactly like everything being fine. The server logs a warning at
startup when it still points at itself, and `trapline doctor` says so too.

Everything else has a default worth living with.

---

## VPS with Docker

From a checkout of the repository:

```sh
cp deploy/.env.example deploy/.env
$EDITOR deploy/.env                    # at minimum, TRAPLINE_ORIGIN
docker compose -f deploy/docker-compose.yml up -d
```

Compose reads `deploy/.env` on every command, including `down`, which is why
the settings go in the file rather than in your shell.

The compose file builds the image locally, because there is no published one
yet. When there is, delete the `build:` block and pin a tag: an image built
from a working copy is not reproducible, and that is a thing to notice on a
server rather than in a postmortem.

The service publishes port 9000 on **loopback only**. Put a proxy in front of
it — the `caddy` profile below, or one the machine already runs.

```sh
docker compose -f deploy/docker-compose.yml logs -f
docker compose -f deploy/docker-compose.yml ps      # STATUS shows the healthcheck
```

### With Caddy, for a certificate that renews itself

```sh
# in deploy/.env
TRAPLINE_DOMAIN=errors.example.com
TRAPLINE_ORIGIN=https://errors.example.com
```

```sh
docker compose -f deploy/docker-compose.yml --profile caddy up -d
```

`deploy/Caddyfile` is an example, mounted read-only, and short on purpose. It
does two things: terminate TLS and pass everything to the app. Point the
domain's A record at the machine first — Caddy asks for the certificate on
first start and will retry loudly if DNS is not there yet.

### There is no shell in the container

The image is `FROM scratch`, so `docker exec … sh` will not work. The CLI is
the interface, and it is a first-class one (ADR 006):

```sh
docker compose -f deploy/docker-compose.yml exec trapline \
  /trapline token create -db /data/trapline.db -name laptop --json
docker compose -f deploy/docker-compose.yml exec trapline \
  /trapline doctor -db /data/trapline.db --quick
```

### Bind mounts need a chown

The container runs as uid 65532. A named volume inherits that ownership from
the image, which is why the compose file uses one. If you would rather have the
database in a directory you can see:

```sh
mkdir -p /srv/trapline && chown 65532:65532 /srv/trapline
```

and swap the volume for `/srv/trapline:/data`. Without the chown the directory
arrives owned by root and the server cannot write to it.

---

## VPS with systemd

```sh
curl -fsSL https://<host>/install.sh | sh -s -- --service
```

The installer downloads the release for the machine's platform, verifies it
against the published checksums, installs the binary in `/usr/local/bin`, and
— only when asked — installs the unit and creates `/etc/trapline/trapline.env`.
Piped into `sh` it never installs a service unasked, because there is nobody
there to ask.

Then:

```sh
sudo $EDITOR /etc/trapline/trapline.env    # TRAPLINE_ORIGIN
sudo systemctl enable --now trapline
journalctl -u trapline -f
```

`deploy/trapline.service` is worth reading before running it. The hardening in
it is not boilerplate: the ingest endpoint is public by design and parses
attacker-chosen bytes, so the unit gives away every capability the process does
not need. It also binds to `127.0.0.1:9000` — the same reasoning as the
container publishing on loopback.

Upgrading is re-running the installer with a new `--version`, then
`systemctl restart trapline`. The schema migrates forward on start.

---

## Behind a proxy you already run

Whatever is in front, three things matter.

**Point `TRAPLINE_ORIGIN` at the public URL**, not at the upstream.

**Do not buffer or truncate request bodies.** Envelopes arrive compressed and
can be large; the server enforces its own hard limit and rejects with a status
an SDK understands. A proxy that returns 413 on its own terms turns a dropped
event into a mystery.

**Give ingest a generous timeout.** SDKs retry, but a retry arrives during the
incident someone is already reading.

<details>
<summary>nginx</summary>

```nginx
server {
    listen 443 ssl;
    server_name errors.example.com;

    # certificates omitted

    location / {
        proxy_pass http://127.0.0.1:9000;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        client_max_body_size 30m;
        proxy_read_timeout   60s;
        proxy_send_timeout   60s;
        # Envelopes are streamed and already compressed; buffering them to disk
        # buys nothing and adds a write per event.
        proxy_request_buffering off;
    }
}
```

</details>

<details>
<summary>Traefik (labels, if trapline runs as a container next to it)</summary>

```yaml
labels:
  traefik.enable: "true"
  traefik.http.routers.trapline.rule: Host(`errors.example.com`)
  traefik.http.routers.trapline.entrypoints: websecure
  traefik.http.routers.trapline.tls.certresolver: le
  traefik.http.services.trapline.loadbalancer.server.port: "9000"
```

Remove the `ports:` block from the compose service if you do this — Traefik
reaches it over the network, and publishing it as well only adds a second door.

</details>

### About `TRAPLINE_TRUSTED_PROXIES`

**Set it if anything sits in front of this server.** Per-IP rate limiting is
real (ADR 023), and this is the setting that tells it who the client is.

List the proxy's address as it appears to trapline — its address on the compose
network, `127.0.0.1` for a proxy on the same host, or a CIDR block for a fleet.
Both IPv4 and IPv6, addresses or CIDR, comma-separated.

- **Left empty behind a proxy**, `X-Forwarded-For` is ignored and every visitor
  is counted as the proxy. The per-IP limit still runs; it just cannot tell
  anyone apart, which looks like a working rate limiter until the day it
  matters. The server says so at startup.
- **Set to something that is not actually in front of you**, the header is
  still ignored, because the check is on the address the connection came from.
- **A typo stops the server**, naming the offending entry. That is deliberate:
  silently accepting it is how an operator ends up believing they configured
  something they did not.

Two related settings, both optional:

```bash
# Ingest requests per minute per client address. Default 48000 — deliberately
# generous, because a backend's whole legitimate volume arrives from one
# address. The per-project limit is what protects the disk.
TRAPLINE_INGEST_IP_RATE_LIMIT=48000

# Login attempts per minute per client address. Default 10 — deliberately
# strict, because every attempt costs a 19 MiB Argon2id working set and this
# installation has exactly one admin.
TRAPLINE_AUTH_RATE_LIMIT=10
```

---

## Backup

The whole installation is one SQLite file, but **do not copy it with `cp`**.
A WAL database copied while it is being written produces a file that looks
fine and is not (ADR 001). There is a command for it, and it uses SQLite's
online backup API:

```sh
# systemd
trapline backup -db /var/lib/trapline/trapline.db -to /backup/trapline-$(date +%F).db

# Docker
docker compose -f deploy/docker-compose.yml exec trapline \
  /trapline backup -db /data/trapline.db -to /data/trapline-$(date +%F).db
docker compose -f deploy/docker-compose.yml cp \
  trapline:/data/trapline-$(date +%F).db ./
```

It never overwrites an existing file, so a backup script that reuses one name
will fail rather than destroy the previous copy.

Restoring is putting the file back where the server expects it, with the
server stopped.

> **The key file.** Alert channels hold secrets — bot tokens,
> webhook URLs — encrypted at rest with a key that lives in a separate file
> next to the database (`<db>.key`, mode 0600), never inside it. That is what
> makes the backup safe to store where the database goes; it also means a
> backup of the database alone cannot decrypt them. **Back up the key file
> once, separately, and keep it somewhere the database backups are not.**
> Losing it does not lose your events — it loses the configuration of every
> alert channel, which then has to be re-entered.

---

## What is deliberately not here

**Automatic TLS outside Caddy.** The `caddy` profile is one working answer, not
a TLS layer inside the product. Terminating TLS is what reverse proxies are
for, and every machine that would run this already has one or can run that one.

**A published image.** There is no registry to publish to yet. The compose file
builds locally in the meantime, and says so.

**Any kind of clustering.** One binary, one file, one machine — that is the
product, not a stage of it (`README.md`, "what it will never do").
