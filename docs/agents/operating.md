# Operar trapline desde un agente

Esta página es para quien no tiene navegador: un agente de código, un script de
CI, una sesión de terminal. Todo lo que hace el panel se hace por aquí, porque
el panel no es más que otro cliente de la misma API — y un feature no está
terminado hasta estar en los cuatro: REST, CLI, panel y MCP
([ADR 006](../adr/0006-una-api-cuatro-clientes.md)).

Hay **dos formas** de llegar, y las dos hablan con lo mismo:

- **La CLI.** `trapline <comando> --json`. No pregunta nada nunca, los códigos
  de salida son contrato, y sirve para administrar una instalación que no
  está en esta máquina.
- **MCP.** `trapline mcp` por stdio, o `POST /mcp` en el servidor. Diez
  herramientas que son, literalmente, llamadas a `/api/v1/`
  ([ADR 022](../adr/0022-mcp-como-cuarto-cliente.md)).

Si tu agente sabe hablar MCP, usa MCP: el bundle de un issue en una llamada es
la diferencia entre arreglar un error y dedicar el turno a reunir lo que hay
que leer antes. Si no, la CLI hace exactamente lo mismo con una tubería.

---

## 1. Lo que necesitas antes de nada

```sh
export TRAPLINE_URL=https://errores.example.com
export TRAPLINE_TOKEN=ek_...
```

El token se crea contra la base de datos, en la máquina del servidor:

```sh
trapline token create -db /var/lib/trapline/trapline.db -name agente --json
```

**Da el scope más pequeño que sirva.** Hay seis, en tres pares, y no son
decorativos: cada par existe porque lo que abre es una autoridad distinta.

| scope | qué abre |
|---|---|
| `projects:read` | proyectos, issues, bundles, stats, releases, transactions, release health |
| `projects:write` | crear proyectos, rotar claves, resolver/ignorar/reabrir issues, subir artefactos |
| `alerts:read` | canales, reglas y el log de entregas |
| `alerts:write` | crear y cambiar canales y reglas |
| `monitors:read` | monitores cron y uptime, sus resultados, la página de estado |
| `monitors:write` | crear y cambiar monitores |

Por defecto, `token create` da `projects:read,projects:write` y **nada más**.
Eso no es un descuido: un canal de alerta guarda credenciales —un bot token,
una contraseña de SMTP—, así que «puede leer la lista de issues» y «puede leer
a dónde manda este servidor sus notificaciones» son permisos genuinamente
distintos. Y un monitor cron guarda una clave de ping, que es una credencial:
cualquiera que la lea puede reportar un backup como correcto desde cualquier
sitio de internet.

Para un agente que sólo diagnostica, `-scopes projects:read`. Para uno que
además cierra issues, el par de `projects`. Los otros cuatro, sólo si va a
tocar alertas o monitores.

Un token sin `projects:write` que intente `resolve_issue` recibe un error de
herramienta que **nombra el 403**, no un fallo genérico: 403 y 404 llevan a
movimientos opuestos, y "falló" lleva a repetir el mismo.

## 2. Códigos de salida

Son parte del contrato público. Se les puede añadir; no se reasignan.

| código | significa | qué hacer |
|---|---|---|
| `0` | salió bien | seguir |
| `1` | error en tiempo de ejecución: el servidor dijo que no, la red falló, el token no vale | leer stderr; reintentar sólo si el mensaje dice que tiene sentido |
| `2` | error de uso: faltaba un flag, sobraba un argumento. **No se intentó nada** | arreglar la invocación; reintentar igual dará lo mismo |

Dos reglas que hacen que esto sea parseable:

- **Los errores nunca van a stdout.** Van a stderr. Un diagnóstico mezclado en
  stdout corrompe justo lo que estás leyendo.
- **`--json` en todo.** Sin él la salida es para una persona y puede cambiar;
  con él es un objeto.

La excepción, dicha en voz alta: `trapline issues bundle` escribe un documento
markdown en stdout tal cual, sin formatear, porque el documento *es* el
resultado. Con `--json` viene envuelto.

## 3. El flujo que importa: de un error a un arreglo

### 3.1 Qué está roto

```sh test
trapline issues list -project 1 -status unresolved --json
```

Filtros: `-q` (texto sobre título y culprit), `-release`, `-environment`,
`-limit`, `-cursor` para paginar. La búsqueda pega contra un índice FTS5 de
issues y **nunca** escanea payloads
([ADR 011](../adr/0011-busqueda-de-texto-con-fts5-sobre-issues.md)).

### 3.2 Todo lo que hace falta para arreglarlo, en una llamada

```sh test
trapline issues bundle -project 1 -issue 1
```

Eso devuelve **un documento markdown**, no un registro: la excepción, el
stacktrace symbolicado con contexto de código y la línea minificada de la que
salió cada frame, los breadcrumbs, los tags y contexts agregados, el ciclo de
vida de release, la frecuencia de 24 h y 14 d leída de los agregados horarios,
y los commits sospechosos con la razón por la que lo son.

Markdown y no JSON porque el lector es un modelo: un documento con encabezados
cuesta menos que un registro cuya forma hay que volver a deducir, y puede decir
**en palabras** lo que un número no dice solo. El render es determinista —los
mismos bytes para el mismo issue— porque la caché de prompt de un agente está
indexada por el texto, y comparar dos bundles es como se sabe si un issue
cambió entre dos lecturas.

Las mismas tres cosas, por las tres vías:

```sh
curl -fsS -H "Authorization: Bearer $TRAPLINE_TOKEN" \
  "$TRAPLINE_URL/api/v1/projects/1/issues/1/bundle"        # REST, text/markdown
trapline issues bundle -project 1 -issue 1                  # CLI
# get_issue_bundle(project: "1", issue: 1)                  # MCP
```

Los tres devuelven **los mismos bytes**. `scripts/mcp.sh` los compara.

### 3.3 Qué cambio lo trajo

```sh test
trapline issues suspects -project 1 -issue 1 --json
```

Cruza los ficheros que tocó cada commit de la release contra los que nombra el
stacktrace, por **sufijo de ruta**, pesando más los frames cercanos a la
llamada que falló. Sin integración con GitHub: las rutas llegan con
`sentry-cli releases set-commits --local` o con `trapline releases commits`
([ADR 019](../adr/0019-suspect-commits-por-interseccion-de-rutas.md)).

Si la release no trae rutas, lista los commits **sin** marcar sospechosos y lo
dice. Un ranking inventado sobre datos que no están es peor que no tenerlo.

Y la atribución es **tan estrecha como el rango** que se subió. Si la release
se cargó sin `-from`, lleva la historia entera del repositorio, y el commit
inicial —que creó todos los ficheros que nombra el stacktrace— gana la
puntuación al commit que de verdad rompió algo. El rango correcto es el de la
release anterior a ésta, que es lo que hace un pipeline de despliegue:
`-from <sha de la release anterior>`.

### 3.4 Cerrarlo

```sh test
trapline issues resolve -project 1 -issue 1 -next-release
trapline issues reopen  -project 1 -issue 1
```

`-next-release` es la diferencia entre "arreglado" y "arreglado, y el arreglo
sale en el próximo deploy". Con él, sólo un evento de una release **más nueva**
reabre el issue; los que sigan llegando de la vieja se cuentan, se guardan y se
enseñan como `seen_in_resolved_release_count`, que es la máquina sin
redesplegar y no una regresión
([ADR 012](../adr/0012-releases-orden-y-resolucion-en-proxima-release.md)).

`trapline issues ignore` silencia sin afirmar que está arreglado.

## 4. Las otras preguntas

```sh test
trapline stats -project 1 -top 5 --json
trapline stats -project 1 -by release --json
trapline transactions list -project 1 --json
trapline releases health -project 1 --json
```

Las dos siguientes son de otras áreas, así que piden sus propios scopes — y
esto es lo que se ve cuando no los tienes:

```sh test
# Con el token por defecto fallan, y el mensaje nombra el scope que falta.
! trapline alerts log --json
! trapline monitors cron list -project 1 --json

# Con uno que sí lo lleva (TRAPLINE_DB es la base, en la máquina del servidor):
OPERADOR="$(trapline token create -db "$TRAPLINE_DB" -name operador \
  -scopes projects:read,alerts:read,monitors:read --json |
  sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
trapline alerts log -token "$OPERADOR" --json
trapline monitors cron list -project 1 -token "$OPERADOR" --json
```

El mensaje dice qué scope falta, con ese nombre, para que el siguiente
movimiento sea pedir el token correcto y no reintentar el mismo.

Todas leen de tablas agregadas, no de eventos. Eso es lo que hace que sigan
respondiendo después de que la retención borre los payloads: los agregados
tienen su propia ventana (400 días por defecto) y los eventos la suya (90)
([ADR 010](../adr/0010-agregados-horarios-en-la-transaccion-de-ingesta.md)).

## 5. MCP: las diez herramientas

Una herramienta **es** una petición a `/api/v1/`. No puede hacer nada que el
token no pueda, no puede saltarse una comprobación de scope, y no puede
responder algo distinto del endpoint — porque es el endpoint.

| herramienta | para qué |
|---|---|
| `list_projects` | los proyectos, con id y slug |
| `list_issues` | los issues de un proyecto, actividad más reciente primero |
| `get_issue` | un issue entero como JSON, con sus eventos guardados |
| `get_issue_bundle` | **el documento markdown de §3.2** |
| `resolve_issue` | marcar arreglado, con `in_next_release` |
| `ignore_issue` | silenciar sin afirmar que está arreglado |
| `reopen_issue` | devolverlo a sin resolver |
| `get_release_health` | crash-free por release |
| `query_stats` | cuánto se rompe y cuándo, por hora o por release/environment |
| `list_transactions` | qué está lento, qué es frecuente y qué falla, con p50/p95/p99 |

`project` acepta **el id o el slug**: resolver un slug cuesta una llamada a
`/projects` con la credencial de quien pregunta, así que no es una forma de
saber que un proyecto existe sin poder leerlo, y ahorra el paso de listar para
leer un número.

### Conectarlo por stdio

El cliente lanza el binario. Nada más:

```json
{
  "command": "trapline",
  "args": ["mcp"],
  "env": {
    "TRAPLINE_URL": "https://errores.example.com",
    "TRAPLINE_TOKEN": "ek_..."
  }
}
```

`trapline mcp` **no abre la base de datos**, aunque esté en el mismo binario:
es un cliente REST como la CLI. Eso es lo que hace que sirva para una
instalación que corre en otra máquina —el caso normal: el agente en el portátil,
el servidor en un VPS— y lo que impide que aparezca una segunda idea de qué
permite cada scope.

Su stdout es el protocolo, no un resultado: los logs van a stderr.

### Conectarlo por HTTP

```
POST https://errores.example.com/mcp
Authorization: Bearer ek_...
```

Sin versionar (la URL de un cliente MCP vive en un fichero de configuración que
un cambio de versión no puede reescribir) y fuera del guardia CSRF (esa defensa
protege cookies, y aquí sólo se acepta `Bearer`). Sin sesiones: `GET` y
`DELETE` sobre esa ruta responden 405.

Es para el agente que **no** está donde está el binario — CI, otra máquina.

## 6. La API REST, si prefieres hablarla directamente

`GET /api/v1/...`, `Authorization: Bearer`. Está congelada desde el 2026-09-20:
se puede añadir, no quitar ni renombrar
([ADR 013](../adr/0013-compatibilidad-con-sentry-cli-a-partir-de-trafico-grabado.md),
[ADR 021](../adr/0021-tracing-sampling-determinista-y-downsampling.md)).

El contrato entero está en [openapi.yaml](../api/openapi.yaml), y no es
documentación de buena fe: `TestRoutesMatchOpenAPI` recorre la tabla de rutas
del binario y el documento **en las dos direcciones**, así que una ruta sin
documentar rompe el build, y una documentada que no existe también.

```sh test
curl -fsS -H "Authorization: Bearer $TRAPLINE_TOKEN" "$TRAPLINE_URL/api/v1/projects" | head -c 200; echo
curl -fsS "$TRAPLINE_URL/api/v1/health"
```

Tres rutas viven **fuera** de `/api/v1/` porque las impone otro protocolo o un
navegador: `/api/{id}/envelope/` (el endpoint de ingesta, público por diseño),
`/ping/{clave}` (el check-in de un cron, sin autenticar porque la clave *es* la
credencial), `/status/{slug}` (la página pública) — y `/mcp`, la única que este
producto pone ahí por decisión propia.

`/api/v1-beta/` sigue respondiendo como alias **deprecado**, con cabecera
`Deprecation`, y desaparece en la siguiente minor. No lo uses en nada nuevo.

## 7. Lo que un agente no debe hacer

- **No hacer `push` ni desplegar.** Ninguna herramienta MCP ni comando de esta
  CLI toca un remoto, y la skill [`/fix-error`](../../skills/fix-error/SKILL.md)
  lo tiene prohibido explícitamente. El arreglo se propone; el deploy lo decide
  una persona.
- **No resolver un issue que no se ha arreglado.** `ignore_issue` existe para
  eso y no miente sobre el estado.
- **No leer la base de datos directamente.** Es un fichero SQLite y se puede
  abrir, pero el servidor está escribiendo en él, y la API es la que aplica los
  scopes.
- **No inventarse endpoints.** Si no está en `openapi.yaml`, no existe.

## 8. Cuando algo no responde

```sh test
trapline doctor --json
```

Mira lo que de verdad se rompe: servidor alcanzable, setup hecho, token válido,
origin que no apunta a sí mismo, jobs de fondo que deberían estar corriendo, y
—si hay canales de alerta— la clave de cifrado en disco.

```sh test
curl -fsS -H "Authorization: Bearer $TRAPLINE_TOKEN" "$TRAPLINE_URL/api/v1/system/jobs"
```

`system/jobs` es la respuesta a "¿qué corre cuando nadie está pidiendo nada?".
Un job que no aparece es un subsistema que nadie encendió, no un fallo: aquí un
subsistema apagado **no arranca su goroutine**
([ADR 005](../adr/0005-toggles-con-backpressure.md),
[ADR 014](../adr/0014-scheduler-con-arranque-condicional.md)).
