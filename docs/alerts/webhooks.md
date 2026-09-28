# Webhooks salientes

Un canal de tipo `webhook` hace un `POST` con el cuerpo JSON de la alerta a la
URL que le des, firmado con HMAC-SHA256.

La firma no es opcional y el secreto tiene un mínimo de 16 caracteres. Sin ella,
el endpoint que recibe las alertas es una URL a la que puede postear cualquiera
que la aprenda —y la aprende cualquiera que vea una petición, un log de proxy o
una captura.

## Crear el canal

```sh
trapline alerts channels add \
  -type webhook -name pager \
  -config '{"url":"https://example.com/trapline","secret":"un-secreto-largo-de-verdad"}'
```

El secreto no vuelve a salir: la API lo devuelve redactado y se guarda cifrado
(ADR 015). Si lo pierdes, crea otro canal.

## Lo que llega

```
POST /trapline HTTP/1.1
Content-Type: application/json
User-Agent: trapline
X-Trapline-Event: new_issue
X-Trapline-Delivery: 4f3c2b1a09876543210fedcba9876543
X-Trapline-Signature: t=1787059200,v1=6c1f…9ab2
```

```json
{
  "event": "new_issue",
  "rule": "anything new",
  "project_id": 2,
  "issue_id": 14,
  "title": "ValueError: invalid amount",
  "culprit": "app/views.py in checkout",
  "level": "error",
  "release": "app@1.0.0",
  "environment": "production",
  "count": 1,
  "url": "https://errors.example.com/projects/2/issues/14",
  "at": "2026-08-29T10:00:00Z"
}
```

`event` es uno de `new_issue`, `regression`, `issue_spike`, `error_rate`,
`cron_missed`, `cron_timeout`, `cron_failed`, `cron_recovered`, `uptime_down`
y `uptime_recovered`. Los campos vacíos se omiten; `baseline` sólo aparece
cuando el disparador comparó contra algo.

Los seis últimos hablan de un **monitor** y no de un issue, así que traen
`monitor_kind`, `monitor_id` y `monitor` en lugar de `issue_id`, `url` apunta
al monitor, y `culprit` dice contra qué se falló — el horario que no se cumplió
para un cron, la URL y el error para un uptime:

```json
{
  "event": "cron_missed",
  "rule": "backups",
  "project_id": 2,
  "monitor_kind": "cron",
  "monitor_id": 3,
  "monitor": "nightly-backup",
  "title": "nightly-backup did not check in",
  "culprit": "0 3 * * * (America/Caracas)",
  "level": "error",
  "url": "https://errors.example.com/projects/2/monitors/cron/3",
  "at": "2026-08-29T07:01:12Z"
}
```

```json
{
  "event": "uptime_down",
  "rule": "uptime",
  "project_id": 2,
  "monitor_kind": "uptime",
  "monitor_id": 3,
  "monitor": "api health",
  "title": "api health",
  "culprit": "https://api.example.com/health — connection refused",
  "level": "error",
  "count": 3012,
  "url": "https://errors.example.com/projects/2/monitors/uptime/3",
  "at": "2026-08-29T07:01:12Z"
}
```

**`monitor_kind` importa**: las dos familias numeran sus monitores en tablas
distintas, así que el monitor cron 3 y el monitor uptime 3 existen los dos y no
tienen nada que ver. Un receptor que guarde estado por `monitor_id` sin mirar
el `monitor_kind` mezclará los dos.

Un receptor que enrute por `X-Trapline-Event` no necesita cambiar para
ignorarlos; uno que asuma que siempre hay `issue_id`, sí.

## Verificar la firma

El formato es `t=<unix>,v1=<hex>`, donde el hex es
`hmac_sha256(secret, t + "." + body)`.

Tres cosas importan y las tres se equivocan seguido:

1. **Firma sobre el cuerpo tal y como llegó**, en bytes. Si lo deserializas y lo
   vuelves a serializar para verificar, cualquier diferencia de orden de claves
   rompe la comprobación —y es así como una firma de webhook acaba documentada
   como "no funciona, desactívala".
2. **Comprueba el timestamp.** Está dentro del material firmado justamente para
   que una petición capturada no valga para siempre. Cinco minutos de tolerancia
   es lo habitual.
3. **Compara en tiempo constante.** `==` filtra la longitud del prefijo que
   coincide, que es suficiente para falsificar un MAC byte a byte con
   suficientes intentos.

### Python

```python
import hashlib, hmac, time

TOLERANCE = 300  # segundos

def verify(secret: str, header: str, body: bytes) -> bool:
    parts = dict(
        p.strip().split("=", 1) for p in header.split(",") if "=" in p
    )
    timestamp, mac = parts.get("t"), parts.get("v1")
    if not timestamp or not mac:
        return False
    if abs(time.time() - int(timestamp)) > TOLERANCE:
        return False

    expected = hmac.new(
        secret.encode(), timestamp.encode() + b"." + body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, mac)
```

### Node

```js
const crypto = require("node:crypto");

const TOLERANCE = 300;

function verify(secret, header, body /* Buffer, sin parsear */) {
  const parts = Object.fromEntries(
    header.split(",").map((p) => p.trim().split("=")),
  );
  const { t, v1 } = parts;
  if (!t || !v1) return false;
  if (Math.abs(Date.now() / 1000 - Number(t)) > TOLERANCE) return false;

  const expected = crypto
    .createHmac("sha256", secret)
    .update(t)
    .update(".")
    .update(body)
    .digest("hex");

  const a = Buffer.from(expected);
  const b = Buffer.from(v1);
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}
```

### Go

```go
func verify(secret, header string, body []byte, now time.Time) bool {
	var timestamp, mac string
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			timestamp = value
		case "v1":
			mac = value
		}
	}
	if timestamp == "" || mac == "" {
		return false
	}

	unix, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if drift := now.Sub(time.Unix(unix, 0)); drift > 5*time.Minute || drift < -5*time.Minute {
		return false
	}

	expected := hmac.New(sha256.New, []byte(secret))
	expected.Write([]byte(timestamp))
	expected.Write([]byte("."))
	expected.Write(body)

	return subtle.ConstantTimeCompare(
		[]byte(hex.EncodeToString(expected.Sum(nil))), []byte(mac)) == 1
}
```

Esta versión de Go no es un ejemplo escrito para el documento: es literalmente lo
que ejecuta `compat/alerts/receiver`, el receptor del gate `scripts/alerts.sh`,
que **no importa nada de trapline** precisamente para que lo que se verifica sea
que alguien puede escribir un receptor leyendo esta página. Un fragmento de
documentación que nadie ha ejecutado nunca es un fragmento que no funciona.

## Entregas repetidas

La entrega es **at-least-once**. Un proceso que muere entre un POST correcto y
el momento de anotar que salió produce una segunda entrega, y eso no se puede
evitar sin un commit distribuido con tu endpoint.

`X-Trapline-Delivery` es estable entre reintentos de la misma notificación:
guárdalo y trátalo como clave de idempotencia.

## Reintentos

Un `2xx` es entrega. Cualquier otra cosa —o un timeout de 10 s— es un fallo, y
se reintenta con backoff exponencial desde 30 s hasta un tope de 1 h, diez veces.
Después la fila queda `dead` y se queda en el log con el último error.

```sh
trapline alerts log -status dead
trapline alerts log -retry 42     # vuelve a la cola con el presupuesto entero
```

Responder rápido importa más que responder bien: encola y contesta. Un endpoint
que tarda 20 s en procesar cada alerta ocupa el notifier mientras lo hace.

## `base_url`, y por qué no lo necesitas

Los canales de Telegram, Slack y Discord aceptan un `base_url` que sustituye la
raíz de la API del proveedor. Existe **para los tests** —el gate apunta los tres
al mismo receptor local, que imita sus rutas reales— y está documentado aquí para
que nadie lo descubra en el código y lo tome por una feature. Una instalación
real lo deja vacío. El canal `webhook` no lo tiene: su URL es su URL.
