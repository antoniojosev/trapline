# Installing trapline

What you install is **one binary and one file**. There is no database to
provision, no queue, no cache, no second process. The entire state of the
installation is one SQLite file you can copy ([ADR 001](adr/0001-sqlite-como-unico-estado.md)).

This page is the path from zero to your first visible error. For production
(systemd, Docker, proxy, TLS, backups) the page is [deploy/README.md](../deploy/README.md),
and it is tested: `scripts/install-test.sh` installs a real release on a clean
Debian every nightly.

---

## 1. Get the binary

**With the installer**, which verifies the archive against the published
checksums before writing anything:

```sh
curl -fsSL https://<host>/install.sh | sh
```

Piped into `sh` it **never** installs a service unless asked to: there is
nobody to prompt. To install the systemd unit as well, `sh -s -- --service`.

**From source**, which is a single step because there is no CGO
([ADR 009](adr/0009-sqlite-puro-go-sin-cgo.md)):

```sh
CGO_ENABLED=0 go build -trimpath -o trapline ./cmd/trapline
```

**With Docker**, if you would rather not think about where a binary lives:

```sh
docker compose -f deploy/docker-compose.yml up -d
```

The image is `FROM scratch` (the binary and a certificate bundle, nothing
else), so there is no shell inside and no distribution to patch.

## 2. Start it

```sh
trapline serve -addr 127.0.0.1:9000 -db ./trapline.db -origin https://errors.example.com
```

Of everything that can be configured, **`-origin` is the only thing you have
to set by hand**. It is the public address the SDKs will send to, and it cannot
be guessed: behind a proxy the request arrives claiming to come from loopback,
while the DSN the panel shows has to name the real domain. Get it wrong and
every DSN works on that machine and on no other, which looks a lot like
everything being fine. The server warns at startup when the origin still points
at itself, and `trapline doctor` repeats the warning.

The schema migrates forward at startup. There is no migration step.

## 3. Create the admin account

The panel lives at the root (`http://127.0.0.1:9000/`) and the first visit asks
you to create the only user there is. The same thing, without a browser:

```sh test
curl -fsS -X POST "$TRAPLINE_URL/api/v1/setup" \
  -H 'Content-Type: application/json' \
  -H 'X-Trapline-Request: 1' \
  -d '{"username":"antonio","password":"una contraseña larga y buena"}' \
  || echo "ya estaba hecho: /setup se cierra después de usarse una vez"
```

`POST /api/v1/setup` **closes after its first use**: a freshly started
installation reachable from the internet is not one where anyone can create the
admin account.

The `X-Trapline-Request: 1` header is the CSRF guard. Every write authenticated
by **cookie** requires it; writes authenticated by `Authorization: Bearer` do
not, because there is no ambient credential to protect there
([ADR 030](adr/0030-cors-abierto-solo-en-ingesta.md)).

## 4. A token for the CLI and for your agent

The token is created **against the database**, not against the server: it is
what you have when you still have nothing.

```sh
trapline token create -db ./trapline.db -name laptop --json
```

It is shown once. Save it and export the two variables the CLI and the MCP
server use:

```sh
export TRAPLINE_URL=https://errors.example.com
export TRAPLINE_TOKEN=ek_...
```

The default scopes are `projects:read,projects:write`. For a token that only
reads (a dashboard, an agent you do not want closing issues):

```sh test
trapline token create -db "$TRAPLINE_DB" -name solo-lectura -scopes projects:read --json
```

## 5. A project, which is a DSN

```sh test
trapline projects create -name mi-app
```

What it prints **is the DSN**, in the form the official SDKs expect:

```
https://<public_key>@errors.example.com/<project_id>
```

From it the SDK derives the ingest endpoint on its own
(`/api/<id>/envelope/`). There is nothing else to configure.

## 6. Your first error

In your application, the line you already know, with the DSN from above:

```python
import sentry_sdk
sentry_sdk.init(dsn="https://<public_key>@errors.example.com/1", release="mi-app@1.0.0")
```

If you would rather check without touching the application, one event by hand
through the same public endpoint the SDKs use:

```sh test
DSN="$(trapline projects create -name humo)"
KEY="$(printf '%s' "$DSN" | sed -n 's|.*//\([^@]*\)@.*|\1|p')"
PROJECT="$(printf '%s' "$DSN" | sed -n 's|.*/\([0-9]*\)$|\1|p')"
EVENT='{"event_id":"0123456789abcdef0123456789abcdef","timestamp":"'"$(date -u +%Y-%m-%dT%H:%M:%SZ)"'","platform":"python","level":"error","release":"humo@1.0.0","exception":{"values":[{"type":"ValueError","value":"hola"}]}}'
printf '{"dsn":"%s"}\n{"type":"event","length":%d}\n%s\n' "$DSN" "${#EVENT}" "$EVENT" |
  curl -fsS -X POST "$TRAPLINE_URL/api/$PROJECT/envelope/" \
    -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_key=$KEY" \
    --data-binary @-
trapline issues list -project "$PROJECT"
```

## 7. Check that the installation is healthy

```sh test
trapline doctor
```

`doctor` looks at what actually breaks in a new installation: that the server
answers, that the origin does not point at itself, that the token is valid,
that the background jobs that should exist do exist, and, when there are alert
channels, that the encryption key on disk has the right permissions and
decrypts.

The variant the container healthcheck runs makes no network calls:

```sh test
trapline doctor -db "$TRAPLINE_DB" --quick
```

---

## What NOT to do

- **Copy the `.db` with `cp` while the server is running.** A WAL database
  copied mid-write produces a file that looks fine and is not. There is a
  command for this, and it uses SQLite's online backup API:
  `trapline backup -db ./trapline.db -to ./backup.db`.
- **Forget the key file.** Since alert channels exist, their secrets are
  encrypted with a key that lives **next to** the database (`<db>.key`, mode
  0600) and never inside it. Copy it once, separately, and keep it away from
  the database backups ([ADR 015](adr/0015-notificaciones-por-outbox-persistente.md)).
- **Leave `TRAPLINE_TRUSTED_PROXIES` empty behind a proxy.** The per-address
  limits are real ([ADR 023](adr/0023-identidad-del-cliente-y-limites-por-ip.md));
  without that variable everyone counts as the proxy, which looks like a rate
  limiter that works until the day it matters.

## Where to go next

- **Coming from Sentry** → [migrate-from-sentry.md](migrate-from-sentry.md)
- **Going to production** → [deploy/README.md](../deploy/README.md)
- **Operating it with an agent** → [agents/operating.md](agents/operating.md)
- **Want to know what it really costs to run** → [benchmarks/footprint.md](benchmarks/footprint.md)
- **Want to understand why it is built this way** → [architecture/README.md](architecture/README.md)
