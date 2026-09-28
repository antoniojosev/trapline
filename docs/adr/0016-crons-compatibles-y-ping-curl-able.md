# 016 — Crons compatibles con check-ins de Sentry + ping curl-able; uptime con guardia SSRF

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Un error tracker responde a algo que llega. La monitorización de crons es lo
contrario: responde a algo que **no** llega. Es la única función de este
producto cuyo evento interesante es una ausencia, y esa diferencia decide casi
todo lo que hay aquí.

El caso real que la justifica no es exótico. Un backup nocturno deja de correr
—se llenó el disco, caducó una credencial, alguien tocó el crontab— y nadie se
entera hasta que hace falta el backup. Ninguna cantidad de reportes de error
cubre eso, porque el proceso que debía fallar es el que no llegó a arrancar.

Hay dos poblaciones distintas queriendo lo mismo, y ninguna de las dos sirve
para la otra:

1. **Aplicaciones con SDK.** Sentry ya define un item de envelope `check_in`, y
   sus SDKs traen un decorador que lo emite: `@monitor(monitor_slug=…,
   monitor_config={…})`. Ese payload lleva el *schedule* dentro, y el servidor
   de Sentry **crea el monitor a partir de él**. Instrumentar un cron pasa a
   ser una línea. Este producto ya aceptaba el item —y lo tiraba a la basura
   contándolo como `not_yet_stored`.
2. **Scripts que no van a importar nada.** Un `0 3 * * * /usr/local/bin/backup`
   en el crontab de una máquina no va a cargar un SDK de Python, no puede leer
   un fichero de configuración que nadie le pasó y no tiene dónde guardar un
   bearer token. Lo único que va a hacer, y sólo si es de verdad una línea, es
   añadir `&& curl -fsS https://…/ping/abc` al final.

Un producto que sólo cubriera la primera población dejaría fuera exactamente el
caso que más lo necesita, porque los cron jobs viejos son los que llevan más
tiempo sin que nadie los mire.

Hay además un problema de zonas horarias que no es un detalle. Un schedule es
una afirmación de **reloj de pared**: «las tres de la mañana» es un instante
distinto en Caracas y en Madrid, y en la mitad de los países cambia dos veces
al año. Un monitor sin zona propia es un bug esperando a un cambio de horario,
y el síntoma —un backup que empieza a reportarse como perdido sin motivo— no
se parece en nada a la causa.

Y uno de superficie: `/ping/{key}` es el segundo endpoint público y sin
autenticar de este producto, después de la ingesta.

## Decisión

### Crons

**Dos entradas, y las dos importan por razones distintas.**

- **Tablas.** `cron_monitors(id, project_id, slug UNIQUE/proyecto, ping_key
  UNIQUE random 32 hex, schedule_type ∈ {crontab, interval}, schedule, timezone
  IANA, checkin_margin_s, max_runtime_s, status ∈ {ok, missed, timeout, error,
  unknown}, last_checkin_at, next_expected_at, enabled)`;
  `cron_checkins(id, monitor_id, checkin_id, status ∈ {in_progress, ok, error},
  started_at, finished_at, duration_ms, environment)`.
- **(a) El item `check_in` del envelope.** `{check_in_id, monitor_slug, status,
  duration, environment, monitor_config{schedule{type, value, unit},
  checkin_margin, max_runtime, timezone}}` → **upsert** del monitor desde
  `monitor_config`. Es el comportamiento de Sentry: **el SDK declara el
  monitor**. Un check-in de un monitor desconocido **sin** `monitor_config` se
  rechaza en vez de inventar un monitor sin schedule, que existiría y no
  reportaría nunca nada —que se parece a que la función va y es lo contrario.
- **(b) `GET|POST /ping/{ping_key}`**, más `/ping/{ping_key}/start` y
  `/ping/{ping_key}/fail`. **Sin autenticación: la clave es el secreto.**
  Respuesta `text/plain` de una línea, rate-limit por IP, `Cache-Control:
  no-store`. Los pings de un monitor `enabled=0` responden **410**, no 404: un
  script que lleva un año pingueando la misma clave no tiene otra forma de
  enterarse de que alguien apagó su monitor, y un 404 se lee como una errata en
  una clave que no ha cambiado.
- **Parser cron propio en el dominio** (stdlib): cinco campos, `*`, listas,
  rangos, pasos y los alias `@hourly|@daily|@weekly|@monthly`; `Next(after,
  loc)`. Con tabla de casos y **fuzz sobre la propiedad `Next(t) > t`**, que es
  la que hace que el barrido termine.
- **Timezone por monitor, IANA**, nunca un offset fijo. Un offset es un hecho
  sobre un instante; un schedule sobrevive al siguiente cambio de hora.
- **Evaluación de `missed`**: job del scheduler cada 30 s que compara
  `now > next_expected_at + margin`. **`timeout`**: un `in_progress` sin cerrar
  durante más de `max_runtime_s`. El timeout gana al missed cuando ambos son
  ciertos: reportar «no arrancó» un trabajo que arrancó y se colgó manda a
  alguien a buscar en el sitio equivocado.
- **`next_expected_at` es columna**, no cálculo al leer. Nadie mira un backup
  que dejó de correr —ése es exactamente el modo de fallo— así que «qué
  monitores están vencidos» tiene que responderse con un índice desde un job de
  fondo, no evaluando una expresión crontab por monitor y por pasada.
- **Triggers** `cron_missed`, `cron_timeout` y `cron_recovered` al outbox del
  ADR 015, escritos **en la misma transacción** que el cambio de estado del que
  hablan. Un cuarto, `cron_failed`, se añade en el ADR 036 por una razón que
  aquel documenta.
- **Scopes propios `monitors:read|write`.** Un monitor lleva una `ping_key`, y
  esa clave es una credencial: cualquiera que la tenga puede reportar un backup
  como correcto desde cualquier parte de internet. Un token acuñado para un
  dashboard no tiene por qué poder silenciar un monitor.

### Uptime

- **Tablas.** `uptime_monitors(id, project_id, name, url, method ∈ {GET, HEAD},
  interval_s ≥ 30, timeout_s, expected_status_min/max, expected_body_substring,
  follow_redirects, public INTEGER, enabled)`; `uptime_results(monitor_id,
  checked_at, ok, status_code, latency_ms, error)`; retención 90 d.
- **Un job por monitor no**: un job `uptime` que despacha los vencidos con
  concurrencia acotada (semáforo 8).
- **Estado `up|down` con umbral de 2 fallos consecutivos** para `down` y 1
  éxito para `up`. Las transiciones producen los triggers del ADR 015.
- **SSRF**: antes de conectar se resuelve el host y se rechaza si **cualquier**
  IP es loopback, privada (RFC 1918/4193), link-local, multicast o `0.0.0.0/8`,
  salvo `allow_private=1` en el monitor **y** `-uptime-allow-private` global.
  El `Dial` usa la IP ya validada, no vuelve a resolver —si no, entre la
  comprobación y la conexión hay una ventana que un DNS hostil elige. Las
  redirecciones se re-validan.

La parte de uptime se implementó en paralelo con ésta; la decisión se
escribe aquí entera para que exista una sola, y uptime sólo anexa su
medición.

## Consecuencias

- **El item `check_in` deja de ser `not_yet_stored`.** Sigue pasando por el
  limitador y por el interruptor de categoría, así que una instalación que no
  ha encendido `check_in` no paga nada por la función (ADR 005): el envelope
  recibe 429 con `X-Sentry-Rate-Limits`, y los SDKs oficiales dejan de
  enviarlo.
- **Hay un segundo endpoint público y sin autenticar.** Está detrás del mismo
  limitador por IP que la ingesta, y deliberadamente con el mismo número: son
  el mismo tipo de tráfico, y un operador que ya ajustó un techo no debería
  descubrir un segundo con otro default el día que un monitor empieza a
  contestar 429. Su respuesta de refusal **no** lleva
  `X-Sentry-Rate-Limits` —eso es la contrapresión del protocolo y usarla aquí
  le diría a un SDK que dejara de reportar errores porque una línea de crontab
  fue ruidosa (ADR 023).
- **La `ping_key` sale por la API.** Es la única credencial de este producto
  que lo hace, y tiene que hacerlo: toda la función es una URL que alguien pega
  en un crontab, y una clave que sólo se ve una vez significaría que perderla
  cuesta un monitor nuevo y una edición del crontab de un servidor. Lo que la
  protege es el scope, y que lo que permite hacer es reportar que un trabajo
  corrió.
- **Un fallo al encolar la notificación aborta la escritura**, al revés que en
  la ingesta. Allí un evento se guarda aunque su alerta no se encole, porque
  tirar el evento sería la peor mitad del trato; aquí el barrido vuelve a
  pasar en treinta segundos sobre un estado que no se movió, así que la
  decisión simplemente se retoma. Un monitor cuyo estado avanzó en silencio
  por delante de la única notificación que alguien quería es justamente el
  fallo que esta función existe para evitar.
- **El parser es propio y no una dependencia.** Cinco campos con listas, rangos
  y pasos son doscientas líneas y un fuzz; una dependencia serían doscientas
  líneas que no se pueden fuzzear en el sitio donde importa —el input llega de
  `monitor_config`, o sea del endpoint público (ADR 002).
- **No se aceptan nombres de mes ni de día** (`JAN`, `MON`), ni `?`, ni `L`, ni
  `#`. Cada uno es el dialecto de algún scheduler, y la respuesta correcta a
  una línea que no se sabe parsear es decirlo, no adivinar un horario y luego
  reportar un trabajo como perdido a una hora que nadie esperaba.
- **Un rango al revés (`22-3`) es un error, no un envoltorio.** Vixie cron
  tampoco lo acepta, y elegir en silencio una de las dos lecturas posibles para
  un monitor que decide a quién se despierta es el tipo equivocado de amabilidad.
  El mensaje dice cómo escribirlo en dos ítems.
- **El gate `scripts/crons.sh` espera noventa segundos reales, una sola vez.**
  Todas las transiciones de estado son función pura y se prueban con reloj
  inyectado en microsegundos; lo que la espera compra es lo único que un reloj
  inyectado no puede afirmar —que el scheduler, el barrido y el notificador
  están realmente cableados entre sí en el binario que se publica.

## Medición

Tomada el 2026-08-29, sobre el gate real
(`scripts/uptime.sh`, nginx en contenedor, receptor de webhooks aparte).

| Qué | Valor |
|---|---|
| Binario | 14 MB (presupuesto 30 MB); sin cambio medible respecto de `main` |
| Dependencias nuevas | ninguna — el guardia y el checker son stdlib |
| Cobertura `internal/ssrfguard` | **100,0 %** de sentencias |
| Cobertura `internal/domain/uptime.go` | 100 % de sus funciones |
| Cobertura `internal/adapters/uptime` | 86,7 % |
| Cobertura `internal/usecase` (paquete) | 85,7 % |
| Throughput de ingesta (`make bench`) | verde, sin regresión: uptime no toca la ruta de ingesta |
| Coste en reposo de la instalación sin monitores | cero: el job no existe (ADR 005/014), verificado por el gate contra `/system/jobs` |
| Latencia detección → notificación entregada | < 20 s desde el segundo fallo, con tick de 5 s y outbox de 1 s |
| Tiempo real del gate | ~2 min, que es el mínimo que permite el producto: intervalo mínimo 30 s × 2 fallos consecutivos + la recuperación |

Las tres cifras que importan y por qué:

- **100 % en `internal/ssrfguard`.** Es el número que el objetivo fijaba por
  encima del 95 %, y la razón de exigirlo es que en un guardia de seguridad la
  línea sin cubrir es exactamente la que un atacante encuentra. La tabla de
  rangos se prueba por los dos lados de cada frontera (`9.255.255.255` público,
  `10.0.0.0` privado) y con las cuatro formas de esconder una IPv4 dentro de
  una IPv6 —mapeada, compatible, NAT64 y 6to4—, que son cuatro maneras de
  escribir `127.0.0.1` que una comprobación ingenua lee como IPv6 corriente.
- **Ninguna dependencia nueva.** El guardia entero es `net`, `net/netip`,
  `net/url` y `strconv`. Se consideró una librería de clasificación de rangos y
  se descartó: la política *es* el valor de este paquete, y delegarla en un
  tercero la convierte en algo que nadie del proyecto ha leído.
- **Cero coste sin monitores.** Verificado como comportamiento, no como
  intención: el gate consulta `/system/jobs` antes del primer monitor y después
  del último, y falla si el job aparece o si no desaparece.

## Lo que se aprendió implementándola

1. **El `Dial` contra la IP validada no es un detalle, es la decisión.** Sin
   él, comprobar el host y entregar después el *nombre* al dialer pregunta al
   DNS dos veces y conecta con la segunda respuesta. La forma de que sea
   imposible equivocarse es que el dialer viva en el mismo paquete que la
   comprobación y sea lo único que se exporta para conectar: `DialContext`
   resuelve una vez, valida lo que obtuvo y conecta a esa dirección.
   Hay un test que lo fija con un resolver que cambia de respuesta entre la
   comprobación y la conexión.
2. **"Cualquiera de las IPs", no la primera.** Un host que resuelve a una
   dirección pública y a `127.0.0.1` puede conectar a las dos y el resolver no
   promete orden. La regla es rechazar si *cualquiera* es no pública, y es la
   diferencia entre un guardia y un formalismo.
3. **El conjunto rechazado es un superconjunto del que enumera la decisión.**
   016 nombra loopback, privada (RFC 1918/4193), link-local, multicast y
   `0.0.0.0/8`. La implementación añade broadcast, CGNAT (`100.64.0.0/10`) y
   los rangos reservados (`192.0.0.0/24`, `192.0.2.0/24`, `198.18.0.0/15`,
   `198.51.100.0/24`, `203.0.113.0/24`, `240.0.0.0/4`, `2001:db8::/32`,
   `100::/64`). Ninguno es alcanzable desde la internet pública, así que
   bloquearlos es el lado seguro en el que equivocarse, y el doble opt-in sigue
   estando ahí para quien de verdad lo necesite.
4. **Las dos autorizaciones son de dos personas distintas.** Quien opera el
   servidor decide si esta instalación puede entrar en su propia red
   (`-uptime-allow-private`); quien escribe un monitor decide si ese check
   concreto lo necesita (`allow_private`). Con un solo interruptor, activarlo
   para un servicio interno abriría en silencio todos los monitores que alguien
   añada después. El mensaje del rechazo dice **cuál de las dos** falta, porque
   si no el operador no sabe dónde mirar.
5. **El umbral de dos fallos hace falta un `consecutive_failures` persistido.**
   Recalcularlo desde el historial sería una consulta por check sobre una tabla
   que se poda a los 90 días — y la poda acabaría borrando la evidencia, con lo
   que un monitor caído cuatro meses volvería a "up" solo.
6. **`GET` y `HEAD` y nada más.** Cualquier URL que alguien escriba la va a
   pedir este servidor, sin supervisión, para siempre. Un monitor que pudiera
   hacer `POST` sería un efecto secundario programado contra un objetivo que
   elige quien pueda escribir un monitor.
7. **Scopes propios `monitors:read|write`.** Crons los reserva; uptime
   los tuvo que declarar para no montar rutas con `projects:*`. Al fusionar las
   dos ramas para la status page, las constantes de `internal/domain/token.go` y la fila de
   `AllScopes()` eran adiciones idénticas en ambas: quedó una, con las dos
   razones —la clave de ping es una credencial, y `monitors:write` es el
   permiso de hacer peticiones salientes— escritas juntas encima.
8. **Las dos familias son un concepto, no dos.** Se implementaron en paralelo y
   cada una llegó con su propia lista de triggers, su propio `switch`, su
   propio `MonitorURL` y su propio despachador de CLI. Fundirlas al montar la
   status page destapó algo que ninguna de las dos podía ver por su cuenta: los ids salen
   de **dos tablas**, así que el monitor cron 7 y el monitor uptime 7 existen a
   la vez y compartían clave de silencio y enlace. El discriminador
   `domain.MonitorKind` y la clave `monitor:<familia>:<id>` son la corrección;
   está razonada aparte, en el ADR 037.

## Deferido, deliberadamente

- **Sin comprobación de certificado configurable.** Un fallo de TLS es una
  caída real para los usuarios del objetivo, así que un monitor que la ignorase
  reportaría verde algo que el navegador de un cliente rechaza.
- **Sin cabeceras por monitor ni autenticación al objetivo.** Un endpoint de
  salud que necesita una credencial es un endpoint que este producto guardaría
  cifrada para enviarla a una dirección elegida por quien escribe el monitor.
  Si hace falta, es un ADR propio.
- **Sin comprobación desde varias regiones.** Un binario, una máquina; decirlo
  en la status page es más honesto que fingir consenso.
