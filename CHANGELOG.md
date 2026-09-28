# Changelog

Formato basado en [Keep a Changelog](https://keepachangelog.com/es/1.1.0/).
Versionado semántico honesto desde v0: un cambio incompatible sube la minor
mientras estemos en 0.x, y se documenta aquí.

Lo que va en la próxima release **no se escribe en este fichero**: vive en
fragmentos sueltos, uno por cambio, en [`changelog.d/`](changelog.d/).
`make changelog` los muestra ya montados y `make changelog VERSION=vX.Y.Z` los
pega aquí al cortar la release. Está partido para que dos ramas que tocan el
changelog a la vez no tengan que fusionar prosa a mano en cada rebase.

## [v0.1.0] — 2026-09-28

### Añadido
- Esqueleto hexagonal con gates de CI desde el primer commit: gofmt, vet,
  golangci-lint (con reglas depguard de frontera), tests con `-race`,
  govulncheck, build multi-arquitectura, presupuesto de tamaño de binario y
  verificación de DCO.
- Gate de arquitectura como test de Go (`internal/arch`): falla el build si
  `internal/domain` o `internal/engine` cruzan su frontera. Verificado en las
  dos direcciones — se comprobó que falla ante una violación deliberada.
- ADRs 001–009.
- Dominio de proyectos: `Project`, `Key` con rotación sin downtime, y `DSN`
  con parser y round-trip contra la forma que esperan los SDKs oficiales.
- Semilla del motor de eventos: categorías, evento genérico, puerto `Store`
  con escritura en batch y política de retención por categoría.
- CLI con el contrato agent-first: `--json`, sin prompts, exit codes estables
  (0 éxito, 1 error, 2 uso).
- Autenticación del panel (setup de un solo uso, login, logout) con Argon2id y
  sesiones en servidor guardadas como hash del token.
- API REST `v1` con dos credenciales y una sola puerta: cookie de sesión
  para el panel, bearer token con scopes para la CLI, MCP e integraciones.
- Tokens de API con scopes lectura/escritura, expiración opcional, registro de
  último uso y prefijo `ek_` para que un escáner de secretos los detecte.
- CLI completa: `serve`, `backup`, `token create|list|revoke`,
  `projects list|create|delete`, `keys rotate|revoke`, `doctor`.
- `backup` como copia consistente en caliente (`VACUUM INTO`), que se niega a
  sobrescribir.
- `scripts/smoke.sh`: arranca un servidor real, lo configura, lo opera por CLI,
  hace backup y mide memoria. Es el criterio de salida del esqueleto, ejecutable.

### Corregido
- **La huella de memoria publicada no se cumplía.** Argon2id con 64 MiB de
  coste llevaba la RSS de 9,6 MB a 142 MB con un solo login, y no volvía. Se
  bajó al parámetro recomendado por OWASP (19 MiB, t=2) y se devuelve el
  working set al sistema tras la ráfaga, coalescido para que un flood de logins
  no fuerce un scavenge por petición. Reposo ~10 MB, pico transitorio ~50 MB,
  ambas cifras verificadas en CI.
- El orden de los pragmas de SQLite: `journal_mode=WAL` escribe el header, y
  después `auto_vacuum` ya no se puede fijar sin un VACUUM completo.
- Panel web (React + Vite + Tailwind) embebido en el binario con `embed.FS`:
  setup de primer arranque, login, alta y borrado de proyectos, rotación y
  revocación de claves, y el DSN con botón de copiar.
- `install.sh` con verificación de checksum (y aviso explícito si no puede
  verificar, en vez de saltárselo en silencio) y unit de systemd endurecida.
- Configuración de goreleaser para binarios multi-arquitectura firmados.

### Corregido
- Montar el panel en `/` dentro del mismo router hacía dos cosas mal a la vez:
  un path de API inexistente devolvía el HTML del panel, y un método incorrecto
  devolvía 200 en lugar de 405, porque un patrón catch-all casa con todos los
  métodos y gana al específico. Se separó en dos routers por prefijo y los
  errores 404/405 del router ahora salen con la forma de error de la API.
- Toolchain de Go fijado a una versión parcheada y `govulncheck` incorporado a
  `make check`: en 1.26.1 había 14 vulnerabilidades de stdlib alcanzables desde
  rutas que este producto sí usa (crypto/tls, crypto/x509, net/http, net/url).

### Ingesta compatible

### Añadido
- Parser de envelopes puro y fuzzeado (5,8M ejecuciones sin crash), con la
  longitud declarada tratada como una afirmación y comprobada antes de reservar
  un solo byte.
- Decodificador del payload de evento, absorbiendo el polimorfismo real del
  protocolo: timestamps en cuatro formas, tags como objeto o pares, mensajes
  como string u objeto, excepciones y breadcrumbs envueltos o desnudos.
- Algoritmo de grouping versionado, con corpus dorado como suite de tests.
- Ciclo de vida del issue: estados, contadores, y detección de regresión.
- Scrubbing de PII **antes** de persistir, con Luhn para no comerse números de
  pedido al buscar tarjetas.
- Endpoint de ingesta compatible con los SDKs oficiales, con auth por DSN,
  gzip/zstd y backpressure vía `X-Sentry-Rate-Limits`.
- Configuración por proyecto: categorías opt-in (perfil mínimo = solo errores)
  y límite por minuto, con caché de 10s en el limitador.
- Almacenamiento de issues, eventos (payload comprimido con zstd) y tags
  agregados, con retención por lotes.
- API y CLI de issues: `issues list|show|resolve|ignore|reopen`.

### Corregido
- **El middleware CSRF se aplicaba al endpoint de ingesta**, así que ningún SDK
  oficial podría haber entregado un evento jamás — y todos los tests de la API
  pasaban igual. La protección CSRF solo tiene sentido donde hay una credencial
  ambiente (la cookie del panel); la ingesta se autentica con una clave que el
  cliente presenta explícitamente.
- **La huella se rompió otra vez al añadir zstd**: la librería reserva estado
  por núcleo, y en una máquina de 24 hilos eran ~16 MB antes de recibir un solo
  evento. Fijada la concurrencia a 1: de 33 MB a 17 MB en reposo.
- El grouping metía el directorio de release en la identidad, así que cada
  deploy creaba issues nuevos y enterraba la señal de regresión.
- Conflicto de rutas entre el path de ingesta (fijado por el protocolo) y el
  prefijo de la API; resuelto despachando por si el primer segmento es numérico.
- Matriz de compatibilidad con SDKs reales (`make compat`), empezando por el
  SDK oficial de Go, en su propio módulo para que las dependencias de los SDKs
  no entren nunca en el grafo del producto.

### Corregido (encontrado por el SDK real)
- **El grouping fusionaba errores distintos en Go.** Todo error creado con
  `errors.New` comparte tipo (`*errors.errorString`) y, lanzado desde una misma
  función, los mismos frames; sin número de línea (excluido a propósito) eran
  indistinguibles. Ahora el mensaje normalizado forma parte de la huella cuando
  hay stacktrace, y el normalizador trata `db-01`, `pod-abc123` y `worker3` como
  identidad de instancia. El corpus dorado no lo habría encontrado: contenía las
  formas que este proyecto *imaginaba* que envían los SDKs.
- Retención ejecutándose sola: barrido por lotes con pausas entre ellos, dentro
  del propio binario, con `trapline retention` para quien prefiera su propio
  cron. Primer barrido inmediato al arrancar, porque un servidor que estuvo
  caído una semana vuelve con una semana de eventos caducados.
- Gate de throughput de ingesta (`make bench`): 548-747 eventos/s sostenidos
  end-to-end, 6-7× por encima del objetivo declarado. Umbral a 150 ev/s sobre
  el mejor de cinco, para cazar cambios de tipo sin parpadear en un runner
  compartido.

### Corregido (encontrado por el benchmark)
- **La protección anti-spike por defecto contradecía el objetivo de diseño.**
  2000/minuto son ~33 eventos/s, y el ADR 001 declara 100 ev/s: un proyecto al
  volumen prometido habría sido rechazado por el propio límite del producto.
  Ahora se deriva del objetivo en vez de elegirse a mano, y un test fija la
  relación.
- **El ADR 001 describía un batching que nunca se implementó.** Medido: valdría
  ~1,5×, no el salto que sugería, porque domina el coste por sentencia y no el
  fsync. Se corrige el documento en lugar de implementar una optimización
  innecesaria que además abriría una ventana en la que un evento está
  confirmado al SDK pero aún no es durable.
- El ADR 009 se mantiene por una razón distinta a la esperada: decodificar el
  JSON de un evento (238 µs) cuesta más que toda la transacción de SQLite
  (360 µs, ~16 µs por sentencia). El sacrificio de velocidad de driver no está
  en el camino crítico.
- Pantallas de issues en el panel: listado con filtro por estado y búsqueda,
  detalle con stacktrace (frame que falló primero, frames de librería
  colapsados), breadcrumbs, tags agregados, contexto, y triaje desde la UI.
  Enrutado propio de 100 líneas, deep links y botón atrás funcionando.

### Corregido (encontrado por el panel)
- **El panel se veía oscuro para todo el mundo.** Tailwind v4 iza cualquier
  `@theme` a un `:root` incondicional, así que la paleta oscura anidada dentro
  de `@media (prefers-color-scheme: dark)` se aplicaba siempre y la clara
  desaparecía del CSS compilado.
- **`last_release` seguía al último evento ingerido, no al más reciente.** Un
  cliente que estuvo sin conexión entregaba un evento viejo de una build vieja y
  reetiquetaba un issue ya visto en una más nueva. La cabecera decía una release
  y la ocurrencia más reciente debajo decía otra.
- La búsqueda solo miraba el título, así que buscar un nombre de fichero no
  encontraba nada; ahora también mira el culprit.
- `web/tsconfig.tsbuildinfo` estaba versionado: una caché de build que cambiaba
  en cada commit.

### Decidido explícitamente
- **`user.email`, `user.id` y `user.ip_address` NO se borran.** Documentado en
  SECURITY.md con su razón: ese contexto existe para saber a quién le pasó, y la
  decisión ya la tomó quien envía al activar `send_default_pii` en su SDK.
  `-scrub-keys email,ip_address` está ahí para quien quiera defensa en
  profundidad.
- Configuración del perfil de ingesta por API y por CLI (`trapline config
  show|set`): categorías opt-in, límite anti-spike y retención por categoría.
  Los ajustes explícitos y los valores por defecto viajan por separado en la
  respuesta, para que "sin configurar" siga siendo un estado y subir un default
  más adelante alcance a quien nunca eligió otra cosa.
- `internal/wiring`: un único camino de ensamblado, usado por producción y por
  los tests.

### Corregido
- **Que un cambio de configuración surtiera efecto dependía de acordarse de
  llamar a un setter durante el ensamblado.** Funcionaba en producción y no en
  un test que cableaba distinto — el modo de fallo de cualquier regla que se
  cumple recordándola. Ahora el limitador *es* el almacén de configuración, así
  que no existe forma de escribir sin invalidar la caché.
- **`omitempty` borraba la diferencia entre "sin configurar" y "vacío a
  propósito".** El dominio siempre las distinguió; el almacenamiento no, así que
  un proyecto configurado para no aceptar nada volvía a aceptar errores en la
  siguiente lectura.
- Paginación por cursor (keyset) en el listado de issues, contadores por estado
  para toda la página, y filtros por environment, release o cualquier tag.
  Environment, release, level y server name se registran como tags en la
  ingesta, así que un solo mecanismo de filtrado los cubre a todos.
- El panel muestra los contadores en los botones de filtro, permite filtrar por
  environment y carga más issues sin saltos.

### Corregido
- **Pedir un issue inexistente devolvía 500**, y un cursor malformado también:
  errores del cliente reportados como fallo del servidor. Un test recorre ahora
  todos los sentinelas del dominio para que un error nuevo sin mapear no pueda
  pasar desapercibido.

---

### Cierre de huecos

### Añadido (panel)
- `scripts/ui-smoke.sh`: el panel abierto por primera vez en un navegador de
  verdad. Chromium fijado por versión en el contenedor oficial de Playwright
  (`web/e2e/`), contra el binario real sirviendo el panel embebido. Recorre la
  historia entera — setup, login, crear proyecto, copiar el DSN, ingerir con
  `curl`, agrupar cuatro ocurrencias en un issue, leer el stacktrace y los
  tags, resolver, regresión, apagar una categoría y volver a encenderla — y
  comprueba el tema en los dos modos de `prefers-color-scheme`. Job `ui-smoke`
  en CI, con screenshots y traza como artefacto cuando falla.
- `web/src/ProjectSettings.tsx` (`/projects/{id}/settings`): categorías,
  límite anti-spike y retención por categoría desde el panel. Lo heredado se
  marca "(default)" y sólo se envía lo que se tocó, que es lo que impide que
  un formulario convierta cada default en una decisión que nadie tomó.
- `GET /projects/{id}/issues` incluye `project: {id, name}`. La cabecera del
  listado ya no depende de una segunda petición que puede fallar sola o llegar
  después de las filas que titula.
- `config set -categories default` en la CLI, y `null` en la API, para volver
  a heredar el perfil por defecto. Escribir a mano el default de hoy se ve
  igual y congela el proyecto para siempre.

### Corregido (encontrado por el panel en un navegador)
- **"Sin configurar" y "no acepta nada" llegaban al cliente idénticos.** La
  respuesta normalizaba la lista de categorías vacía a `[]`, así que un
  proyecto recién creado — que acepta errores por herencia — y uno apagado del
  todo se serializaban byte por byte igual. `config show` anunciaba "this
  project accepts nothing" sobre un proyecto que estaba aceptando errores sin
  problema, y listaba `error` entre las categorías rechazadas. La CLI estaba
  escrita para el contrato correcto desde el principio: el bug era del
  adaptador HTTP, y arreglarlo no le cambió una línea.
- **El gate que debía cazarlo buscaba "(default)" en cualquier parte de la
  salida** y se conformaba con la línea del rate limit, mientras la de encima
  mentía. Ahora está anclado a la línea que le toca, y comprueba también el
  estado opuesto y la vuelta atrás.
- **El nombre accesible de cada sección del detalle salía pegado**:
  "Stacktracemost recent call first", una palabra que nadie escribió,
  anunciada a quien sólo tiene ese nombre para orientarse. Margen no es
  separación cuando lo que lee el lector es texto.
- Listar los issues de un proyecto que no existe devolvía una página vacía con
  200. Una página vacía es lo que parece un proyecto tranquilo, y un id mal
  tecleado no puede hacerse pasar por uno: ahora es 404, como ya hacía
  el endpoint de configuración por la misma razón.

### Añadido (distribución)
- **Imagen de contenedor `FROM scratch`** (`Dockerfile`): el binario estático,
  el bundle de certificados y nada más. Usuario no-root, `/data` como volumen,
  y un `HEALTHCHECK` que abre la base en solo-lectura y comprueba sus pragmas
  — sin red, y sin crear nada, para que un volumen mal montado se reporte
  enfermo en vez de arrancar en silencio sobre una base vacía.
- `trapline doctor --quick`: diagnóstico local sin llamadas de red, pensado
  para el healthcheck. `-db` acepta ruta o `TRAPLINE_DB`.
- `deploy/docker-compose.yml` con volumen nombrado, rootfs de solo lectura y
  perfil opcional `caddy` (`deploy/Caddyfile`), más `deploy/.env.example`.
- `deploy/README.md`: VPS con Docker, VPS con systemd, detrás de
  Caddy/nginx/Traefik, y cómo se hace backup de verdad — incluido el fichero de
  clave de secretos de los canales de alerta, documentado ahora para que una rutina de
  backup escrita hoy no haya que rehacerla.
- `scripts/bootstrap.sh` (`make bootstrap`): idempotente, lee las versiones del
  propio repo, y termina listando qué falta y el comando exacto que lo arregla
  en vez de fallar a medias.
- `scripts/install-test.sh` (`make install-test`): construye una release real
  con goreleaser en contenedor, la sirve, y la instala desde un Debian limpio.
  Verifica el checksum, el binario, `trapline version`, que el servicio **no**
  se instala sin pedirlo, que sí se instala cuando se pide, y que
  `systemd-analyze verify` acepta el unit. Incluye el caso negativo: un archivo
  manipulado tiene que ser rechazado.
- `TRAPLINE_BASE_URL` en `install.sh`, para instalar desde un mirror, una copia
  air-gapped o la release de prueba.
- `.github/workflows/nightly.yml` con los jobs `install` e `image`.

### Corregido (distribución)
- `install.sh` nunca se había ejecutado. Ahora hay un gate que lo ejecuta
  contra una release de verdad en cada nightly.

### Añadido (matriz de compatibilidad)
- **La matriz de compatibilidad se cierra con seis SDKs.** Se suman navegador
  (`@sentry/browser` en un Chromium real dentro de la imagen de Playwright),
  PHP (`sentry/sentry` en `php:8.3-cli`) y Dart (`sentry`, Dart puro, no
  Flutter, en `dart:3.13.2`), cada uno en su contenedor contra un servidor real
  en el host. Cada suite manda errores deliberadamente distintos — el mismo
  error repetido no prueba grouping, prueba conteo.
- `scripts/compat.sh --tier smoke|full`: los runtimes que un desarrollador de Go
  ya tiene en cada PR, la matriz entera de noche. Un gate lento es un gate que
  alguien apaga, y un gate apagado es peor que ninguno (ADR 002).
- `.github/workflows/nightly.yml`: matriz completa, fuzz de 10 minutos (con el
  corpus subido como artefacto si encuentra algo) y benchmark.
- `compat/README.md` publica la tabla de la matriz con versiones pinneadas y
  fecha, y notas por SDK de lo que cada ecosistema hace distinto.
- Casos nuevos en el corpus dorado, en ambas direcciones, con las formas que
  solo mandan los SDKs nuevos: URLs completas como ruta de frame (mismo asset
  por http y https agrupa; misma ruta en dos hosts no), `file://` frente a ruta
  absoluta para el mismo fichero, huecos `<asynchronous suspension>` de Dart, y
  dos errores distintos desde una misma función minificada.

### Corregido (encontrado por el SDK real)
- **La ingesta no servía política CORS, y el fallo era invisible desde este
  lado.** Un POST cross-origin con body plano no necesita preflight: el
  navegador mandaba todos los eventos, el servidor los guardaba y respondía
  200, y nada parecía roto — mientras el navegador impedía a la página leer una
  sola respuesta. En la respuesta va la contrapresión del protocolo:
  `X-Sentry-Rate-Limits` es cómo se le dice a un SDK que deje de mandar una
  categoría apagada, y es la razón entera de que un subsistema apagado no
  cueste **en el cable** (ADR 005). Todo SDK de navegador era sordo a eso, y una
  configuración que sí hiciera preflight no habría entregado nada, sin rastro
  en ningún lado. Ahora la ingesta —y solo la ingesta, que no tiene credencial
  ambiente— permite cualquier origen, expone las cabeceras de límite y responde
  al preflight. La suite de navegador lo verifica preguntando lo que el SDK no
  puede: manda un envelope de sesión desde la página y exige un 429 legible con
  su cabecera de límite legible.
- **Un error de navegador no registraba en qué navegador ocurrió.** "Solo en
  Safari" es la respuesta a buena parte de los bugs de front-end, y el SDK no
  puede ayudar porque una página no tiene un nombre fiable de sí misma. El dato
  viaja en la única cabecera que escribe el navegador en cada petición, y se
  estaba tirando. Ahora `User-Agent` se lee a `contexts.browser`, con un parser
  pequeño y deliberado: los tokens se prueban de más específico a más prestado
  (Edge dice ser Chrome, Chrome dice ser Safari), un Chromium headless se
  reporta como tal y no como Chrome, y lo que no se reconoce no se inventa.

### Cambiado
- El binario compilado de la suite de Go dejó de estar versionado: 10 MB de
  ELF que entraron por accidente en el repo. `compat/go/.gitignore` evita que
  vuelvan.

### Añadido (límites por IP)
- **Rate limit por dirección de cliente**, que `SECURITY.md` prometía y no
  existía (ADR 023). Dos limitadores con propósitos distintos, no uno
  reutilizado: la ingesta con un techo holgado y derivado del volumen de diseño
  de ADR 001 (48 000 req/min, `-ingest-ip-rate-limit`), aplicado **antes de leer
  y decodificar el body** porque el coste que se evita es precisamente ese; y
  `/setup` y `/login` con un techo estricto (10/min, `-auth-rate-limit`) porque
  lo que se raciona ahí no es ancho de banda sino 19 MiB de Argon2id por
  intento.
- `internal/clientip`: paquete puro que decide de quién es una petición. Con
  `-trusted-proxies` (IPs o CIDR, ambas familias) recorre `X-Forwarded-For` de
  derecha a izquierda saltando hops de confianza; sin él, o desde un peer que no
  está en la lista, **ignora el header por completo**. Es una frontera de
  seguridad y está en el gate de `internal/arch` y en `depguard`, con el 100 %
  de sus líneas cubiertas.
- Tope global de **2 hashes Argon2id en vuelo**. El límite por dirección no
  acota la memoria por sí solo: mil direcciones cada una por debajo de su techo
  llegan igual a la vez.
- Los mapas de los limitadores están **acotados y desalojan bajo presión**
  (16 384 entradas para ingesta, 4 096 para auth), y en IPv6 se cuenta la /64:
  rotar direcciones dentro de un /64 es gratis, así que un limitador que no lo
  hiciera sería decorativo y su propio mapa sería la vía más barata de agotar el
  proceso. Al llenarse se pierde precisión, nunca estabilidad (mismo criterio
  que ADR 008).
- Un 429 por IP lleva `Retry-After` y **no** `X-Sentry-Rate-Limits`. Ese header
  hace que los SDKs oficiales dejen de enviar esa categoría durante la ventana:
  usarlo como defensa convertiría un límite en pérdida de datos.
- `scripts/smoke.sh` hace un flood de 40 logins concurrentes con
  `X-Forwarded-For` forjados **justo antes** de medir el presupuesto de memoria,
  así que la cifra que publica el gate es la de un servidor bajo ataque.

### Corregido (límites por IP)
- **`/setup` y `/login` no tenían ningún límite y cada intento costaba 19 MiB.**
  Medido: 200 logins concurrentes llevaban la RSS del proceso a **1,03 GB**,
  treinta y cuatro veces el presupuesto publicado de 30 MB, sin autenticarse y
  sin ancho de banda. Con el límite por dirección y el tope de hashes en vuelo,
  la misma prueba da **52,9 MB**. El gate de memoria existía desde el primer commit y no lo
  cazó porque medía el pico de **un** login: respondía "¿cuánto cuesta un
  login?" cuando la pregunta del atacante es "¿cuánto cuestan mil?".
- **`TRAPLINE_TRUSTED_PROXIES` se parseaba, se validaba a medias y no lo
  consumía nadie**, mientras `SECURITY.md` describía un comportamiento
  completo. Ahora se consume, y una entrada mal escrita **detiene el arranque**
  nombrándola: aceptarla en silencio dejaba a un operador creyendo que había
  configurado algo que no, con un síntoma —cada visitante contado como el
  proxy— idéntico al de un rate limit que funciona.
- **`SECURITY.md` describía dos defensas que no existían.** Reescrito para
  decir lo que el código hace, con una sección nueva de **lo que no protege** y
  con marca explícita en las decisiones que aún describen código no escrito.

### Añadido

- **`GET /api/v1/system/jobs`**: qué corre de fondo, con `last_run`,
  `last_error` y `next_run`. Un job que lleva una semana fallando se ve igual de
  sano que uno sano desde cualquier otra cosa que se le pueda preguntar a un
  servidor. `trapline doctor` lo muestra también, en texto y con los campos
  intactos en `--json`.
- Sólo aparecen los jobs **en marcha**. Un subsistema apagado no sale listado
  como «inactivo»: no existe, que es exactamente lo que promete el ADR 005.
- `internal/scheduler` (ADR 014): un job se *ofrece* junto con la pregunta de si
  su subsistema tiene trabajo, y sólo los que responden que sí llegan a tener
  goroutine. La pregunta se rehace cuando cambia la configuración, así que
  encender un subsistema surte efecto sin reiniciar y apagarlo devuelve el
  goroutine. Un `Run` que falla —o que hace panic— se registra, se publica y no
  detiene el resto.

### Corregido

- **`PUT /projects/{id}/config` respondía 200 para un proyecto que no existe**,
  y devolvía un cuerpo indistinguible del de un guardado correcto. Un id mal
  tecleado se hacía pasar por una operación que había escrito algo. Ahora
  responde 404, como ya hacía la lectura por la misma razón.

### Cambiado

- La retención pasa a correr dentro del scheduler. El comportamiento visible es
  el mismo —primer barrido al arrancar, cada hora, por lotes— pero un barrido
  que falla ahora se cuenta y se publica en lugar de quedarse en el log.

### Añadido

- **El dashboard, como API y como CLI.** `GET /projects/{id}/stats` da la serie
  por hora con el desglose por nivel; `/stats/top` los issues con más eventos en
  el rango; `/stats/breakdown?by=release|environment` a quién culpar; y
  `/projects/{id}/issues/{issueID}/stats?range=24h|14d` el gráfico de un issue
  concreto. Lo mismo desde la terminal: `trapline stats -project N
  [-from -to] [-top N] [-by release|environment] [-issue N] [--json]`. Las horas
  vacías vienen en la respuesta como ceros, no como huecos: un gráfico dibujado
  sólo con las horas que tuvieron eventos convierte una noche tranquila en una
  línea recta entre dos picos. (La pantalla del panel llega en el bloque
  siguiente.)
- Las cifras salen de **agregados por hora escritos junto con el evento**, así
  que responder no cuesta un escaneo y **la historia sobrevive a los payloads**:
  los buckets se guardan 400 días frente a los 90 de los eventos. Preguntar "¿esto
  está peor que el trimestre pasado?" sigue teniendo respuesta cuando los eventos
  de entonces ya no están.
- La hora de un evento es **cuándo ocurrió**, no cuándo llegó. Un móvil que estuvo
  sin red aparece en la hora en que se rompió, no en la que recuperó la conexión.
- `retention_days` acepta ahora la categoría **`aggregates`**, que es lo único con
  ventana propia que ningún SDK puede enviar. `config show` la lista junto a las
  demás.

### Cambiado

- **La búsqueda del listado (`q`, y `trapline issues list -q`) pasa por un índice
  de texto completo.** Encuentra **cualquier trozo** de lo que busca, no sólo el
  principio: `Error` encuentra `PaymentError`, `Timeout` encuentra
  `ConnectionTimeoutError` y `SQLSTATE` encuentra `SQLSTATE[23000]`, que es como
  se busca de verdad cuando los nombres de excepción son palabras compuestas.
  Ignora los acentos en ambos sentidos —"función" y "funcion" son la misma
  búsqueda— y exige **todos** los términos, no cualquiera de ellos. Además del
  título y del culprit, ahora también busca en el mensaje del último evento del
  issue.
  Una consecuencia que conviene saber: un término de **menos de tres caracteres**
  no se puede buscar, porque el índice está hecho de ventanas de tres. Si va junto
  a otro término más largo se ignora ("de", "la"); si es lo único que escribiste,
  la búsqueda se rechaza con un mensaje que lo dice —y `trapline issues list -q ab`
  sale con código 1— en vez de devolver una lista vacía que parecería "no hay
  nada".
- **Una retención de 0 días ahora significa no conservar nada**, y ya no "usar el
  valor por defecto". Antes era un ajuste que se podía escribir, leer de vuelta y
  ver cómo no hacía nada, y no había forma de pedir que se purgara una categoría.
  Para volver a heredar el valor por defecto se omite la categoría, o se usa
  `trapline config set -retention default`, que limpia todos los overrides de
  golpe. **Si tenías un 0 guardado esperando el comportamiento anterior, esa
  categoría empezará a purgarse en la siguiente barrida.**

### Corregido

- Un rango de fechas inválido en un endpoint de estadísticas respondía **500 en
  vez de 400**, y con "internal error" en el cuerpo: el error del dominio no
  estaba en la tabla de traducción a estados HTTP, así que un `from` mal escrito
  se le presentaba al cliente como un fallo del servidor.

### Añadido

- **Resolver un issue «en la próxima release»**: `trapline issues resolve
  --next-release`, o `{"status":"resolved","in_next_release":true}` en la API.
  A partir de ahí, los eventos que llegan de la release en la que se resolvió
  —o de una anterior— se cuentan y se guardan, pero **no** reabren el issue.
  Sólo un evento de una release posterior cuenta como regresión. La API expone
  cuántos se suprimieron en `seen_in_resolved_release_count`, para que se vea
  que no se han perdido.
- **Releases como entidad**: `GET/POST /projects/{id}/releases`,
  `GET/PUT /projects/{id}/releases/{version}` (el `PUT` finaliza),
  `POST …/releases/{version}/commits` y `POST …/releases/{version}/deploys`.
  En la CLI: `trapline releases list|create|show|finalize|commits|deploys`.
- Un evento que nombra una release desconocida **la crea**. No hace falta
  registrar nada para que las releases aparezcan: registrarlas explícitamente
  sirve para poder finalizarlas, asociarles commits y anotar despliegues.
- El detalle de una release dice **cuántos issues introdujo** (`new_issues`),
  **cuántos trajo de vuelta** (`regressed_issues`) y **cuántos eventos produjo**
  (`events`), además de sus commits y sus despliegues. El conteo de eventos sale
  de los agregados horarios, así que sigue siendo correcto después de que la
  retención borre los eventos que lo formaron, y cuenta los de esa release —no
  los del proyecto entero—. Una release de la que todavía no ha llegado ningún
  error reporta cero.
- Los commits admiten `patch_set` con las rutas que tocaron. Se guardan para
  poder responder más adelante «qué commit causó este error» sin clonar el
  repositorio.
- El detalle de un issue añade `first_release` (en qué release nació),
  `regressions` (cuántas veces ha vuelto), `regressed_in_release` (qué
  despliegue lo trajo de vuelta la última vez) y el estado de la resolución.

### Cambiado

- **Un issue resuelto y luego reabierto ya no pierde el rastro**: se cuenta la
  regresión y se recuerda en qué release ocurrió. Reabrir un issue **a mano**
  sigue sin contar como regresión, que es lo que hace que el número signifique
  algo.
- Las releases se ordenan por versión semántica cuando lo son (`myapp@1.2.3`,
  `1.2.3`, con pre-release) y, cuando no lo son —un sha de git, un número de
  build—, por el orden en que esta instalación las vio por primera vez. Una
  release que nunca se ha visto se considera la más nueva.

### Añadido

- **`sentry-cli` funciona contra este servidor para el ciclo de releases.**
  `sentry-cli releases new`, `releases set-commits --local`,
  `releases finalize` y `releases deploys new` corren sin cambiar nada del
  pipeline salvo `SENTRY_URL` y `SENTRY_AUTH_TOKEN` — el token propio (`ek_…`)
  vale como `SENTRY_AUTH_TOKEN`. La release, sus commits con las rutas que
  tocaron y su despliegue quedan visibles en la API propia, en la CLI y en el
  panel, igual que si los hubiera registrado `trapline releases`.
- **Los proyectos tienen slug**: derivado del nombre, único, visible en
  `trapline projects list`, en `GET /projects` y junto al nombre en el panel.
  Es lo que se pone en `SENTRY_PROJECT`. El id numérico se sigue aceptando en
  todas partes; una configuración que ya usaba el entero no cambia.
- **La superficie emulada se construyó grabando tráfico real, no leyendo
  documentación** (ADR 013). `compat/sentry-cli/record.sh` levanta un servidor
  grabador y ejecuta el `sentry-cli` pinneado contra él; las peticiones quedan
  commiteadas en `compat/sentry-cli/fixtures/<versión>/` y son lo que el
  handler implementa y contra lo que se prueba.
- **Gate nuevo `make sentry-cli`** (`scripts/sentry-cli.sh`, nocturno): graba
  si faltan fixtures para la versión pinneada, reproduce las peticiones
  grabadas contra el handler y después corre el `sentry-cli` real, en
  contenedor, contra una instalación real — comprobando por la API propia que
  la release, sus commits y su deploy quedaron donde debían.

### Cambiado

- `/api/0/` deja de contestar 404 y pasa a ser la superficie emulada. El
  reparto de `/api/` no se tocó: cero nunca fue un id de proyecto válido, así
  que la rama nueva entra delante sin mover nada de lo que ya había.
- A diferencia de la API propia, la superficie `/api/0/` **ignora los campos
  que no conoce** en vez de rechazarlos. Los campos de ese cable los elige otra
  herramienta y ganan miembros entre sus versiones; rechazar el primero
  desconocido rompería un despliegue por una actualización ajena.

### Añadido

- **Un dashboard**. La pantalla nueva de un proyecto (`/projects/{id}`) dibuja
  los errores por hora desglosados por nivel, los issues más ruidosos del rango
  —que no son los mismos que los más recientes— y el reparto por release y por
  environment, con selector de 24 h, 7 días y 30 días. Todo sale de los
  agregados horarios, así que la historia sigue ahí después de que la retención
  borre los eventos que la produjeron.
- **Pantallas de releases**. La lista dice qué ha salido y cuándo se vio por
  primera y última vez; el detalle, cuántos issues introdujo ese despliegue,
  cuántos trajo de vuelta, cuántos eventos produjo, y sus commits y deploys.
- **Resolver «en la próxima release» desde el panel**, con la distinción
  visible: un issue resuelto sin más y uno resuelto a partir del próximo
  despliegue no se leen igual. El segundo dice en qué release se ancló y
  cuántos eventos de esa release se han contado **sin** reabrirlo, en vez de
  parecer que el producto los perdió.
- **«¿Es nuevo o volvió, y qué release lo trajo?» se responde en pantalla.** El
  detalle de un issue abre con una franja que lo dice con palabras —primera
  aparición, o regresión— y nombra la release, enlazada a su detalle.
- **Feed en vivo**. Mientras alguien mira la lista de issues, el panel mantiene
  abierto `GET /projects/{id}/events/stream` (server-sent events, cookie o
  bearer) y avisa con un banner de «N issues nuevos». La lista **no** se
  reordena sola: durante un incidente eso mueve la línea que se estaba a punto
  de pulsar. Se recarga cuando quien lee lo pide.
- **Búsqueda y filtros en la lista de issues**: el buscador con retardo, y
  filtros por release y environment que viajan en la dirección, de modo que
  «esto sólo pasa en producción» es un enlace que se pega en un chat y abre la
  lista ya filtrada.
- **Cualquier tag es un filtro**. En el detalle de un issue, cada valor de tag
  enlaza a la lista acotada a él.
- **El navegador es ahora un tag**: `browser.name` (`Safari`) y `browser`
  (`Safari 17.4`). Ya se registraba como contexto, pero un contexto no se puede
  filtrar, así que «¿esto sólo pasa en Safari?» se podía leer issue a issue y no
  se podía preguntar a la lista.
- **Sparklines de 24 h** junto a cada issue, en la lista y en el detalle. Un
  único `GET /projects/{id}/stats/series?issues=…` para toda la página, también
  disponible en la CLI (`trapline stats -issues 1,2,3`), que la dibuja con
  bloques en el terminal.

### Corregido

- **Ningún contenedor de la máquina podía alcanzar al servidor**, mientras `ss`
  lo mostraba escuchando tan tranquilo. Arrancado sin nombrar un host
  (`-addr :9000`), sólo se ataba al comodín IPv6, y el relay de localhost de
  Docker Desktop sobre WSL2 replica únicamente los sockets atados a IPv4. Ahora
  «todas las interfaces» significa un socket por familia, y una dirección que
  nombra una familia (`0.0.0.0:9000`) recibe esa (ADR 034).
- **Los gates que levantan un servidor comprobaban el puerto sólo por
  costumbre**: si algo ya estaba escuchando, hablaban con ello y fallaban mucho
  después con un síntoma que no se parecía en nada a la causa —el caso real fue
  un `invalid credentials` en el gate de estadísticas, que era el script
  hablando con un servidor de otra rama—. Los cinco abortan ahora nombrando el
  puerto.
- Una búsqueda demasiado corta para el índice ya no se presenta como «no hay
  resultados». El servidor siempre distinguió las dos cosas; el panel las
  mostraba igual, que es como se pierde una tarde buscando un bug en los datos
  propios.

### Cambiado

- `-addr 0.0.0.0:9000` deja de aceptar conexiones IPv6, que es lo que esa
  dirección pide. Quien quiera ambas familias tiene `-addr :9000`, que ahora las
  da de verdad.

### Corregido
- **La retención borraba eventos que estaban dentro de la ventana que prometía
  conservar.** Los instantes se guardaban como texto con `time.RFC3339Nano`,
  que suprime los ceros finales de la fracción, así que un instante en segundo
  exacto se escribía sin fracción y `.` es menor que `Z` en ASCII: SQLite leía
  `10:00:00.5Z` como anterior a `10:00:00Z`. Con el corte de retención en un
  segundo exacto —que es como cae un corte, días enteros restados a un
  instante— un evento medio segundo **posterior** al corte se consideraba
  anterior y se borraba, sin ningún síntoma posterior. Los segundos exactos no
  son un caso raro: son lo que manda cualquier SDK que redondee.
- El mismo orden textual gobernaba el `ORDER BY last_seen` de la pantalla
  principal (un issue visto en un segundo exacto se hundía por debajo de otro
  medio segundo más viejo), el cursor keyset de paginación (que se saltaba o
  repetía filas alrededor de un segundo exacto) y el ensanchado de
  `first_event_at` / `last_event_at` de una release con `MIN()` y `MAX()`.
- Un `timestamp` numérico suficientemente grande en un evento (`1e17`) producía
  un instante fuera de los años de cuatro dígitos, que se escribía con cinco
  dígitos de año y volvía a romper el orden de todas las demás filas. Ahora se
  trata como cualquier otro timestamp inservible y responde el reloj del
  servidor.

### Cambiado
- **Los timestamps se guardan con ancho fijo** (fracción siempre presente, nueve
  dígitos), de modo que el orden textual y el cronológico coinciden para todo
  par de instantes por la forma del dato y no por una afirmación en un
  comentario (ADR 033). La **migración 0010** reescribe las 22 columnas de
  tiempo del esquema; es idempotente y deja intacto lo que no tiene forma de
  timestamp. `parseTime` sigue leyendo el formato anterior, porque un binario
  abre y migra la base en la misma llamada y un backup restaurado puede ser más
  viejo que el esquema.
- Los cursores de paginación emitidos antes de la migración dejan de casar con
  las columnas reescritas. Son opacos y viven lo que una sesión de listado; el
  producto no tiene instalaciones.

### Añadido

- **Alertas: canales, reglas y entrega.** Cinco canales — Telegram, Slack,
  Discord, webhook firmado y email por SMTP — y cuatro disparadores sin lenguaje
  de consulta: `new_issue`, `regression`, `issue_spike` y `error_rate`. Romper
  una aplicación y recibir el aviso, con enlace al issue, deja de requerir que
  alguien esté mirando el panel (ADR 015).
- **La notificación se escribe en la misma transacción que el issue del que
  habla**, y la envía después un job `notifier` con backoff exponencial de 30 s a
  1 h. No hay ventana entre "el issue existe" y "hay algo que va a avisar de él",
  que es donde cae un reinicio — y un reinicio es lo primero que hace un operador
  cuando algo va mal, o sea el momento exacto en que un tracker con la cola en
  memoria se queda mudo.
- **Ventana de silencio persistida, por sujeto.** Un deploy que rompe dos
  endpoints produce dos avisos y luego calla sobre ambos. En memoria se perdería
  en cada reinicio, y el reinicio ocurre durante la ráfaga.
- **Credenciales de canal cifradas en reposo** (AES-256-GCM), con la clave en
  `<db>.key` (0600), generada al primer uso o fijada con `-secret-key-file` /
  `TRAPLINE_SECRET_KEY_FILE`. La API devuelve la configuración redactada.
- **`trapline backup` avisa de que el `.key` no viaja dentro del `.db`**,
  nombrando la ruta, en texto y en `--json`. Sin ese aviso, una restauración deja
  los canales listados y mudos sin nada en los logs que apunte a la causa.
- **`trapline doctor --quick` comprueba la clave**: permisos, y que descifra un
  canal real. Sólo si hay canales, y nunca la crea.
- API: `alerts/channels` (CRUD + `POST .../test`), `alerts/rules` (CRUD +
  `POST .../test` con un issue sintético), `GET alerts/notifications?status` y
  `POST alerts/notifications/{id}/retry`.
- CLI: `alerts channels add|list|test|remove`, `alerts rules add|list|remove|test`
  y `alerts log`, todos con `--json`.
- Scopes nuevos `alerts:read` y `alerts:write`. Un canal guarda credenciales de
  sistemas ajenos, así que leer a dónde manda sus avisos una instalación no es lo
  mismo que leer su lista de issues. **Un token creado antes de este build no los
  tiene**: `trapline token create` sigue dando `projects:*` por defecto y hay que
  pedirlos con `-scopes`.
- Gate `scripts/alerts.sh`: cinco canales contra destinos reales (mailpit en
  Docker para SMTP, y un receptor propio que imita las rutas de Telegram, Slack y
  Discord y verifica el HMAC con código escrito sólo desde
  `docs/alerts/webhooks.md`). Comprueba la entrega en menos de cinco segundos, el
  silencio, la recuperación tras una caída del otro extremo y que un `kill -9`
  del servidor a mitad no pierde ni duplica nada.
- `docs/alerts/webhooks.md`: formato de la firma y verificación en Python, Node y
  Go.

### Cambiado

- El job `notifier` sólo existe si hay al menos un canal configurado, y aparece o
  desaparece sin reiniciar el servidor (ADR 005, ADR 014). `GET /system/jobs` lo
  muestra sólo cuando está en marcha.
- `SECURITY.md`: la firma HMAC de los webhooks salientes deja de estar marcada
  como no implementada, porque ya lo está.

### Corregido

- `.github/workflows/ci.yml` tenía el job `stats` con nombre y sin cuerpo: sus
  pasos habían quedado bajo `releases` en un rebase anterior, lo que dejaba el
  fichero sin la clave `runs-on` obligatoria. Un workflow que no parsea no
  ejecuta ningún job, incluidos los que sí estaban bien.
- El texto de ayuda de la CLI listaba dos veces la línea de comandos remotos, con
  dos listas distintas y ninguna completa.

### Añadido

- **Digest semanal.** Un resumen por instalación, por proyecto, que dice cuántos
  errores hubo esta semana comparado con la anterior, qué issues aparecieron,
  cuáles volvieron y cuáles fueron los cinco más ruidosos. Sale de los agregados
  horarios y de dos columnas indexadas de `issues`, así que sigue siendo
  respondible sobre una semana cuyos payloads la retención ya borró — que es
  precisamente el rango en el que un resumen semanal vive.
- **`POST /digest/preview` y `trapline digest preview`**, que renderizan el
  resumen sin enviarlo. Existe porque la otra forma de saber qué dice un correo
  semanal es esperar una semana, y porque previsualizar es lo que alguien hace
  *antes* de configurar a dónde mandarlo. El `--json` trae los mismos números
  como campos, no como prosa (ADR 006).
- **Día y hora configurables**: `GET|PUT /system/settings/digest` y
  `trapline digest schedule [-day monday -hour 9]`. Por defecto, lunes a las
  09:00 UTC. Sólo UTC en esta versión, dicho explícitamente en la respuesta en
  vez de insinuado.
- **El job `digest` sólo existe si algún canal lo pidió** (ADR 014). No es un
  goroutine dormido comprobando una bandera: sin canal con digest activado no
  se registra, no aparece en `GET /system/jobs` y no consume nada. La primera
  vez que se enciende no envía un resumen retroactivo: marca el periodo y espera
  al siguiente, porque un correo sobre una semana que nadie estaba mirando es
  cómo se consigue que alguien apague la función.
- **`doctor` comprueba los canales de alerta sin enviar nada.** Para cada canal
  dice dos cosas por separado: si la clave de secretos abre su configuración, y
  si esta máquina llega a donde entrega —resolución de nombre, conexión y
  handshake TLS incluidos, sin una sola petición—. Un canal mal configurado se
  descubría el día que algo se rompía, que es el peor día para descubrirlo. Lo
  que un probe no puede afirmar (que la credencial siga siendo válida) `doctor`
  lo dice con esas palabras en vez de dejarlo implícito.
- **Tabla `settings`** (migración 0016): clave y valor JSON para la
  configuración de instalación. La usa el digest y la reutilizarán la status
  page y el MCP, en vez de una tabla y una migración por cada dato
  suelto que pertenece a la instalación y no a un proyecto.
- **Fixtures dorados del digest** (`internal/digest/testdata/digest/*.golden`),
  y `internal/digest` como paquete puro verificado por el gate de fronteras. Un
  correo semanal no falla con un error: se deforma poco a poco hasta que nadie
  lo lee. Los fixtures hacen que cualquier cambio de forma sea deliberado o sea
  un test en rojo.
- **`scripts/digest.sh`** y el job `digest` en CI: dos semanas de historia
  ingeridas por el endpoint público, y luego cada afirmación que hace el
  resumen. Termina borrando los eventos y volviendo a pedirlo.

### Corregido

- **El workflow de CI no era válido y no corría ningún job.** Al mergear la rama de estadísticas,
  el job `stats` se quedó sin `runs-on` ni `steps` —sus pasos habían caído
  dentro de `releases`— y GitHub Actions rechaza el fichero entero, no sólo ese
  job. Trece gates verdes en local y cero ejecutándose en la nube, sin que nada
  lo dijera. `stats` y `releases` vuelven a ser dos jobs.

### Cambiado

- **`issues.regressed_at`** (migración 0017): cuándo volvió un issue, no sólo
  cuántas veces. El contador bastaba para la pantalla de un issue, que sólo
  pregunta "¿ha vuelto esto?"; no basta para nada que pregunte por un periodo.
  Sin la columna, la respuesta honesta a "qué volvió esta semana" exige leer
  eventos (el escaneo que el ADR 001 prohíbe, y que deja de funcionar cuando la
  retención los borra) y la barata es falsa: un issue que volvió en marzo y
  lleva desde entonces dando guerra se reportaría como noticia de esta semana.
- **`domain.Origin.IsLocal()`**: la regla de "este origen apunta a esta máquina"
  vivía en `config` y ahora vive en el tipo. La pregunta se hace en todos los
  sitios donde el producto entrega una URL para usar en otro lado — un DSN, y
  ahora los enlaces dentro del correo semanal. Una instalación que nunca
  configuró `-origin` recibe un digest sin enlaces, en vez de uno lleno de
  `http://127.0.0.1` que el lector pulsa una vez y luego desconfía del resto.

### Añadido

- **Pantalla de alertas en el panel** (`/alerts`), una para toda la instalación
  y no una por proyecto: un workspace de Slack no es una propiedad de una
  aplicación, y dos copias del mismo canal bajo dos proyectos es el arreglo que
  manda cuatro mensajes idénticos en un solo incidente. La regla es la que
  elige su alcance. Con esto el subsistema de alertas cumple la paridad del
  ADR 006 — REST, CLI y panel — y deja de ser algo que sólo se enciende con una
  terminal abierta, que es justo lo que nadie tiene durante un incidente.
- **Alta de canales con el formulario de su tipo.** Cinco tipos con cinco
  formas distintas, y el formulario es el del tipo elegido en vez de veinte
  campos de los que diecinueve sobran. Cada canal trae un botón **Probar**, que
  entrega un mensaje real —la única comprobación que demuestra la credencial,
  porque un token de bot sólo se prueba usándolo—, y un botón **Comprobar
  canales**, que resuelve, conecta y cuelga sin enviar nada. Están separados a
  propósito: un diagnóstico que escribe "test" en el canal del equipo se ejecuta
  una vez.
- **Builder de reglas sin DSL**: cuatro condiciones con nombre y sus números,
  que se persisten como el JSON del trigger tal cual (ADR 015). Cambiar de
  condición reemplaza el trigger entero en vez de mutar su `kind`, porque una
  regla que arrastra un `factor` que su tipo nunca lee la escribió alguien que
  esperaba algo que no va a pasar — y la API la rechaza, con razón, después de
  que esa persona se pregunte por qué.
- **Log de entregas con reintento**, filtrable por estado. Cada fila dice en una
  oración qué significa el suyo, porque `failed` y `dead` se leen como sinónimos
  y son opuestos: uno se reintenta solo y el otro no volverá a intentarse a
  menos que alguien lo pida. Confundirlos es alguien esperando un aviso que no
  va a llegar nunca.
- **Ajustes del digest en el panel**: día y hora, sobre la tabla `settings`. Y
  cuando el digest no está programado, la pantalla dice **por qué** no lo está
  y qué hay que hacer, en vez de dejar a alguien buscando un job que el
  scheduler nunca registró (ADR 005/014).
- **`scripts/ui-smoke.sh` levanta el receptor de webhooks** en la red del gate,
  y `web/e2e/smoke.spec.ts` prueba el flujo de alertas entero desde el
  navegador: crear el canal con su secreto, crear la regla, romper una app y
  recibir el aviso firmado con enlace al issue. El enlace se **sigue**, no se
  compara: una URL con la forma correcta y el issue equivocado es idéntica en
  una comparación de cadenas e inútil a las tres de la mañana. Medido: 1,0 s,
  con el presupuesto en 60.

### Cambiado

- **El digest entrega por los canales reales.** El digest dejó la frontera del
  ADR 035 escrita y sin cablear porque el outbox se construía en otra rama;
  ahora el repositorio de canales implementa `ports.AlertChannels` y
  `wiring.Options.Channels` desaparece — una opción con una sola implementación
  sólo sirve para ensamblar un servidor cuyo resumen semanal no tiene a dónde
  ir. El job del digest arranca en el acto al marcar la casilla en un canal y
  desaparece con el último que lo quería, sin reinicio.
- **`doctor` deja de decir que no hay subsistema de notificaciones.** Cuando no
  hay canales dice que no hay canales, que es la verdad de una instalación que
  no ha querido alertas, y sigue en verde: no configurar alertas es una
  decisión, no una avería.
- **El endpoint de un canal es un destino al que conectarse, no su URL de
  entrega.** `GET /system/channels` está guardado por `projects:read`, un scope
  más débil que el `alerts:read` del listado de canales, y para Telegram, Slack
  y Discord la URL de entrega **es** la credencial: el token va en el path, y el
  path de un incoming webhook es toda su autenticación. Ahora sólo sobrevive
  esquema, host y puerto para esos tres — que es exactamente lo que la sonda
  necesita. El webhook firmado conserva su URL, porque se autentica por HMAC.
- **`SECURITY.md`** dice ahora qué devuelve `GET /system/channels` y por qué no
  es la URL de entrega, que es la mitad de la promesa de "lo que entra como
  secreto no vuelve a salir" que faltaba por escribir.
- **`internal/wiring` tiene tests propios**, que era deuda pendiente: el cable
  que conecta escribir un canal con que el scheduler reevalúe sus jobs estaba
  probado por sus dos extremos y por ninguno de los dos en conjunto. Un cable
  desconectado se ve exactamente igual que uno conectado, hasta que alguien
  marca una casilla y no pasa nada hasta el siguiente arranque.

### Añadido
- **Los errores de front-end dejan de llegar minificados.** Un evento cuyo
  frame decía `bundle.min.js` línea 3 columna 489 en una función llamada `t`
  ahora dice `../src/checkout.js` línea 10, con la línea de código y cinco de
  contexto a cada lado. Se resuelve **al ingerir**, por dos vías y en este
  orden: el `debug_id` que el SDK pone en `debug_meta` —que es el camino por
  defecto de `sentry-cli sourcemaps upload` y funciona sin release— y, si eso
  no encuentra nada, el par `release + dist` con la URL del frame (ADR 018).
- `internal/sourcemap`: parser de Source Map v3, sólo stdlib, con `sections`
  aplanado al parsear y las extensiones `x_facebook_*` deliberadamente
  ignoradas. Vive en su propia frontera —no importa nada del repo, lo verifican
  el test de arquitectura y `depguard`— porque lee un fichero que sube un
  usuario. Fuzzeado con `make fuzz-sourcemap`: 60 s, 23,9 M ejecuciones, cero
  fallos.
- Caché LRU de mapas parseados acotada **por bytes** (64 MB por defecto,
  `-sourcemap-cache-mb` / `TRAPLINE_SOURCEMAP_CACHE_MB`). Por bytes y no por
  número de mapas porque el tamaño de un mapa lo elige quien sube: una caché de
  «200 mapas» es una caché cuya memoria decide otro. Recuerda también los
  fallos, con precio, para que un front-end que nunca subió mapas no consulte
  la base de datos una vez por evento.
- El frame original se conserva en `frame.raw` — fichero, función, línea y
  columna minificadas. Es lo único que se puede comparar contra el bundle
  desplegado cuando alguien sospecha que el mapa está viejo.
- Tabla `artifacts` (migración **0024**) y su repositorio de lectura, con el
  esquema corregido según el tráfico grabado de `sentry-cli`: `kind` es
  `minified_source` / `source_map` —lo que dice el manifiesto— y hay sitio para
  la cabecera `sourcemap` que une script y mapa, sin la cual la vía legacy es
  adivinar. **La API de subida se describe más abajo**; aquí los artefactos se cargan por el
  repositorio.
- El decodificador de eventos lee `debug_meta.images` y `dist`, y ambos
  sobreviven al almacenamiento: un mapa subido mañana se puede comprobar contra
  los ids de un evento guardado hoy.
- Gate de coste: un evento de **12 frames todos resolubles** —peor caso, peor
  que un stacktrace real— no puede costar más del 10 % de la ruta de ingesta.
  Medido: la symbolication conserva el 85–88 % de la capa de dominio, que es
  ≈ 4–5 % del total. Se mide ahí y no sobre el socket porque sobre el socket la
  cifra oscilaba veintiséis puntos con el código sin tocar, por el WAL de
  SQLite; la capa de dominio no tiene disco y es la medida más estricta de las
  dos.

### Cambiado
- **Grouping usa el frame symbolicado cuando existe.** No cambia
  `GroupingVersion` y no re-agrupa nada —el fingerprint sólo se calcula al
  ingerir (ADR 003)—, pero tiene una consecuencia que conviene saber antes de
  encender los mapas: **el mismo error minificado de antes y el de después
  pueden quedar como dos issues**. Es un corte de una sola vez por proyecto. La
  alternativa —agrupar por el frame minificado y enseñar el resuelto— cuesta un
  issue nuevo en cada build, para siempre.
- Un source map que no se encuentra, no se lee o no se parsea cuesta la
  symbolication y **nunca** el evento: una subida equivocada no puede parar el
  error tracking de un front-end entero. El parseo fallido sí avisa por `WARN`,
  porque el único otro síntoma sería silencio.

### Añadido
- **Subir source maps con `sentry-cli` funciona apuntando `SENTRY_URL` aquí.**
  `sourcemaps inject && sourcemaps upload` sube un artifact bundle por debug
  id, con o sin `--release`; `releases files <v> upload` sigue funcionando para
  los pipelines que aún usan el camino viejo. Se implementó contra tráfico
  grabado del `sentry-cli` real y pinneado, no contra documentación
  (ADR 013, ADR 018).
- `trapline artifacts upload -project <id> [-release <v>] [-dist <d>] <dir>`,
  más `artifacts list` y `artifacts delete`: subir source maps **no** requiere
  instalar `sentry-cli`. Construye el mismo archivo y lo manda a la API propia,
  así que los dos caminos acaban en el mismo lector. No inyecta debug ids —eso
  reescribiría el build— pero lee los que ya estén, en sus dos grafías. Si no
  hay ni debug id ni `-release`, avisa y no sube: sin ninguna de las dos claves
  nada podría volver a encontrar esos ficheros.
- API propia: `GET/POST /projects/{id}/artifacts` y
  `DELETE /projects/{id}/artifacts/{artifactID}`, con filtros por `release`,
  `dist`, `debug_id` y `name`. El listado dice también cuánto ocupa el proyecto
  y cuánto le queda, que es lo único que hace falta saber cuando una subida
  empieza a fallar.
- Presupuesto por proyecto: `artifacts_max_mb`, 200 MB por defecto, en
  `config show`/`config set -artifacts-max-mb`. **Cero significa cero** —no
  aceptar ninguna subida contra ese proyecto— y `"default"` vuelve a heredar.
- Tablas `artifacts` (migración **0024**) y `artifact_chunks` (**0025**). Los
  artefactos subidos contra una release se borran **con ella**; los que sólo
  llevan debug id, a los 30 días. Los chunks de una subida interrumpida
  caducan a las 24 h. Todo lo barre el pase de retención que ya existía, sin un
  temporizador nuevo.
- `internal/artifactbundle`: lector y escritor del ZIP con `manifest.json`,
  puro y fuzzeado (`make fuzz-bundle`), dentro del gate de fronteras.
- `scripts/sourcemaps.sh` y el job `sourcemaps` de la CI nocturna: el
  `sentry-cli` real conducido por inject, upload por debug id, upload con
  release y dist, el camino viejo, un presupuesto excedido y la CLI propia — y
  después la API de este producto preguntada por lo que llegó. Incluye un caso
  negativo que apunta la misma herramienta a un servidor que contesta "ok" sin
  guardar nada, y exige que la comprobación del gate no encuentre nada.

### Corregido
- **`trapline help` imprimía dos veces el párrafo de "Remote commands"**, con
  dos listas distintas y las dos incompletas.
- Pasarse del presupuesto de artefactos, o subir bytes que no son un ZIP,
  respondía **500** en vez de 413 y 400 — y el 500 esconde precisamente el
  mensaje que dice qué cambiar.

### Añadido
- **Commits sospechosos.** Un issue nuevo dice ahora qué cambio lo trajo, no
  sólo en qué release apareció: se cruzan los ficheros que tocaron los commits
  de su primera release con los ficheros que nombra su stacktrace, y se señalan
  los tres mejores con su razón — «tocó `src/checkout.ts`, que es el frame #1».
  Un frame más cercano a la llamada que falló pesa más, y un empate lo decide el
  commit más reciente (ADR 019). Sin integración con GitHub, sin clonar nada y
  sin credenciales de terceros.
- `GET /api/v1/projects/{id}/issues/{issueID}/suspects`, `trapline issues
  suspects -project <id> -issue <id> [--json]` y la sección «Suspect commits»
  del detalle del issue: las tres a la vez (ADR 006).
- **`trapline releases commits -repo . -from <sha> -to <sha>`**: lee los commits
  de un checkout local con `git log --name-status` y los sube con sus rutas.
  Existe porque las rutas son toda la entrada de la atribución, y `sentry-cli
  releases set-commits` sólo las manda con `--local` — una bandera que medio
  mundo olvida y cuya ausencia sólo se nota semanas después, cuando la página
  del issue no señala nada. Un rename se guarda como dos hechos: la ruta nueva
  añadida y la vieja borrada.
- **El stacktrace se puede ver minificado.** Un frame resuelto por un source map
  guarda la posición original del bundle en `raw` desde el primer resolutor; el panel ahora
  tiene el interruptor que la enseña. Es lo único que permite comparar contra el
  build desplegado cuando se sospecha que el mapa está mal o viejo (ADR 018).
- **La página de una release lista sus artefactos**: los scripts y los mapas
  subidos para ese deploy, con su debug id y su tamaño, y cuánto lleva gastado
  el proyecto de su presupuesto. Cuando no hay ninguno lo dice con el comando
  que los sube, porque «no hay sospechosos» y «nadie subió los mapas de este
  build» son la misma historia contada desde dos pantallas.
- Cuando no se puede atribuir nada, se dice por qué y qué cambiar: la release no
  tiene commits, los commits llegaron sin rutas, o los frames siguen
  minificados. Un «ningún commit coincide» a secas manda a la persona a buscar
  un fallo en la puntuación en lugar de a su pipeline.
- `scripts/sourcemaps.sh` amplía el gate con la cadena entera: un repositorio
  git de verdad con tres commits, uno de los cuales toca `src/checkout.ts`, un
  error lanzado desde el bundle minificado, y el issue resultante enseñando el
  código original y señalando ese commit. Y el caso sin `patch_set`, que tiene
  que listar los commits **sin** marcar ninguno.

### Añadido

- **La grabación de `sentry-cli` cubre ahora los source maps.**
  `compat/sentry-cli/record.sh` graba cuatro flujos más contra el
  `sentry-cli` pinneado: `sourcemaps upload` resuelto sólo por debug id,
  el mismo con `--release` y `--dist`, el `releases files … upload` heredado, y
  lo que hace la herramienta contra un servidor que no ofrece artifact bundles.
  Las peticiones quedan commiteadas en
  `compat/sentry-cli/fixtures/<versión>/<flujo>/`, cada flujo en su directorio.
- **`record.sh --check`**: vuelve a grabar en un directorio temporal y compara
  con lo commiteado, tolerando sólo las tres fechas que `sentry-cli` estampa con
  su propio reloj. Una grabación que nadie puede reproducir no dice si el
  protocolo sigue igual.
- **Una fixture de evento con `debug_meta` real**, capturada de un Chromium de
  verdad sobre un bundle minificado con debug ids inyectados:
  `compat/browser/record-envelope.sh` la graba y la deja, junto al bundle y al
  mapa exactos a los que apunta, en `internal/sourcemap/testdata/`.
- **El protocolo de subida de `sentry-cli`, grabado contra Sentry**: la
  secuencia exacta de peticiones, qué campos exige en cada respuesta para dar
  una subida por buena, el formato del artifact bundle, y en qué no encajaba el
  ADR 018 con lo grabado (recogido en su anexo).

### Corregido

- El binario compilado del grabador (9 MB) estaba commiteado desde que existe el grabador, así que
  `go build ./...` dentro de su módulo dejaba el repo sucio y nadie podía tener
  un `git status` limpio. Fuera del índice y en `.gitignore`.

### Añadido
- **Tracing: el item `transaction` deja de descartarse.** Cada transaction que
  llega se cuenta, se clasifica como correcta o fallida y su latencia se pliega
  en un histograma fusionable. Los percentiles se calculan al consultar,
  fusionando las ventanas del rango; **nunca se guarda un percentil**
  (ADR 007, ADR 020, ADR 021).
- `internal/engine/sketch`: histograma logarítmico fusionable tipo DDSketch,
  sólo stdlib, con **1 % de error relativo** garantizado y **fusión exacta**.
  20 000 latencias log-normales caben en 860 bytes. Vive dentro de la frontera
  del motor y no importa nada del resto del repo, lo que verifican el test de
  arquitectura y `depguard`. `UnmarshalBinary` está fuzzeado, porque lee bytes
  que vienen de disco (ADR 020).
- Decodificador de transactions en `internal/sentry/transaction.go`:
  `transaction`, `start_timestamp`, `timestamp`, `contexts.trace{trace_id,
  span_id, parent_span_id, op, status}` y `spans[]`, con las dos formas de
  timestamp que los SDKs mezclan y un techo de 1000 spans. Fuzzeado.
- Sampling determinista **por trace** (`hash(trace_id) < rate`), configurable
  con `traces_sample_rate` por proyecto (default 0,1). Todas las transactions
  de una petición comparten destino, así que un waterfall se guarda entero o no
  se guarda. Si el SDK ya sampleó —lo dice el objeto `trace` de la cabecera del
  envelope o la cabecera HTTP `baggage`— las tasas componen, y la efectiva se
  muestra observada en cada respuesta (ADR 021).
- Tablas `txn_minute` (48 h), `txn_hour` (retención `transaction`, 7 d por
  defecto) y `traces` (sólo los muestreados), migraciones **0022** y **0023**.
- Job `downsample` del scheduler, registrado sólo si algún proyecto acepta
  transactions: funde los minutos cerrados hace más de 2 h en su hora y los
  borra. La fusión es exacta, así que los percentiles no se mueven. Se puede
  correr a mano con `trapline downsample -age <duración>`.
- API: `GET /projects/{id}/transactions?from&to&sort=p95|count|fail`,
  `GET /projects/{id}/transactions/{name}/series?resolution=minute|hour` (con
  los waterfalls guardados de esa transaction al lado) y
  `GET /projects/{id}/traces/{trace_id}`.
- CLI: `transactions list|show` y `traces show`, que dibuja el waterfall en el
  terminal con una barra por span. `config set -traces-sample-rate`, con
  `"default"` para volver a heredar.
- Retención real de la categoría `transaction`: las horas y los waterfalls
  guardados van en la ventana del proyecto, los minutos en la fija de 48 h.
- `scripts/tracing.sh` y el job `tracing` de CI: 500 transactions por el SDK
  oficial de Go con latencias de distribución conocida —cuyos percentiles
  exactos calcula el emisor antes de serializarlas— y comprobación de que p50,
  p95 y p99 caen dentro del 2 %. Con `rate=0.1`, que se guarde ~10 % de traces
  y **los agregados sigan contando el 100 %**. Con el pliegue forzado, que los
  tres percentiles no se muevan.
- El gate de throughput mide ahora dos cargas: errores y transactions, con el
  mismo umbral. Medido: **1884 transactions/s** contra 586–648 eventos/s.

### Corregido
- **El sampling de traces no repartía.** Tomar los 53 bits altos de un FNV-1a
  crudo dejaba a cuatrocientos trace ids que comparten prefijo —un fixture, un
  generador secuencial, una librería con epoch fijo— todos del mismo lado del
  umbral: con `rate=0.1` no se guardaba **ni un solo trace**. Se añade el
  finalizador `fmix64` de MurmurHash3 antes de tomar los bits. El fallo era
  invisible en una muestra de veinte mil ids y lo cazó la suite HTTP.
- `domain.ErrTraceNotFound` no estaba en el mapeo de estados de la API, así que
  pedir un trace que nadie guardó respondía **500** en vez de 404 — que es
  precisamente la distinción que necesita alguien que siguió un trace id desde
  una línea de log.

### Añadido
- **Release health: el item `session` deja de descartarse.** Cada session que
  llega mueve cuatro contadores por (release, environment, hora), y la respuesta
  a «¿qué tal va esta release?» es un crash-free rate calculado al consultar.
  Los items `session` (un update de una session) y `sessions` (agregados que el
  SDK ya sumó, que es lo que manda el SDK de Python en modo request) entran por
  el mismo camino (ADR 008, ADR 021).
- **Jamás una fila por session.** Contar sessions distintas exige estado porque
  un SDK no manda sessions terminadas, manda *updates* de una — un `init` y
  luego un `exit` o un `crashed`. Ese estado es una **ventana acotada en
  memoria**: 50 000 entradas con LRU y TTL de una hora
  (`internal/domain/release_health.go`, una estructura de datos pura, sin reloj
  y sin lock). Al cerrarse, expirar o ser desalojada, una session incrementa
  contadores; los contadores se escriben cada 60 s **y en el apagado**.
- Tabla `session_hourly`, migración **0026**, con los cuatro contadores
  **disjuntos** (`started = healthy + errored + crashed + abnormal`) igual que
  el propio item `sessions` del protocolo. La hora es la de **inicio** de la
  session, no la del update que la cerró.
- Decodificadores de sessions en `internal/sentry/session.go`, para las dos
  formas del protocolo, con las dos formas de timestamp que los SDKs mezclan y
  un techo de 1000 buckets por item agregado. Fuzzeados (`make fuzz-session`).
- Job `session-flush` del scheduler, registrado sólo si algún proyecto acepta
  la categoría `session` (ADR 005, ADR 014).
- API: `GET /projects/{id}/releases/{version}/health?from&to` →
  `{started, errored, crashed, abnormal, healthy, crash_free_rate, series,
  window}` y `GET /projects/{id}/health`, que ordena todas las releases del
  rango por volumen y responde la pregunta con la que se abre la página: cuál
  de ellas es la mala.
- CLI: `trapline releases health -project N [-version V] [-from] [-to]
  [-limit]`, con `--json` como todo lo demás.
- Flag `-session-window` / `TRAPLINE_SESSION_WINDOW` para el techo de la
  ventana, por si una máquina tiene menos memoria de la que este producto
  asume.
- Retención de `session_hourly` en la ventana de `aggregates` (400 días): estas
  filas *son* el resumen que sobrevive a lo que lo produjo.
- `scripts/health.sh` y el job `health` de CI: 100 sessions por el SDK oficial
  de Python con `auto_session_tracking`, 5 crasheadas y 10 con un error
  manejado, contra un **0,95 exacto**; una hora ya escrita idéntica byte a byte
  tras un `kill -9` mientras desaparecen las 65 que estaban en la ventana; y
  1 000 sessions abiertas contra una ventana de 100, que la deja llena y no más
  llena, con el servidor respondiendo y las 900 desalojadas todavía contadas.

### Cambiado
- Las respuestas de health llevan un objeto `window` con `in_flight`,
  `capacity`, `evicted`, `expired` y la frase que dice en voz alta lo que la
  ventana cuesta: **las cifras de la hora más reciente pueden cambiar tras un
  reinicio**. La CLI la imprime; el panel la mostrará. Es la mitad
  documentada del trade-off del ADR 008 — una sorpresa documentada no es una
  sorpresa. Y cuando la ventana se ha llenado, añade que las cuentas son una
  cota inferior.
- `crash_free_rate` es `null`, y no 1, cuando no hubo sessions. Decir «100 %
  correcto» cuando la verdad es «nadie ha reportado» esconde la más alarmante
  de las dos situaciones.
- Una parada limpia drena la ventana antes de terminar (`app.Serve`, después de
  que el servidor HTTP haya dejado de aceptar). Un `kill -9` no, y eso es lo
  que el ADR 008 acepta perder.

### Cambiado
- **La API REST se congela: `/api/v1-beta/` pasa a ser `/api/v1/`.** Se puede
  añadir una ruta, una respuesta puede ganar un campo y una petición puede ganar
  un campo opcional; nada se quita, se renombra ni cambia de significado sin un
  prefijo de versión nuevo (ADR 006, anexo). Cada ruta y cada superficie pública
  están escritas a mano en `docs/api/openapi.yaml`, y `TestRoutesMatchOpenAPI`
  compara ese documento con la tabla de rutas del binario en las dos
  direcciones: una ruta servida y no documentada falla, y una documentada que ya
  no existe falla también — que es la peor de las dos, porque es un contrato
  publicado que miente.
- **`/api/v1-beta/` sigue respondiendo una minor más**, con `Deprecation: true`
  y `Link: </api/v1>; rel="successor-version"`, incluidos los 404 de rutas que
  ya no están: a quien está migrando le sirve más «deprecada, y no» que un 404 a
  secas. Después desaparece — un alias que nunca anuncia su final es una segunda
  API permanente.
- La superficie `/api/0/` que habla `sentry-cli` **no** se congela ni se
  versiona, y no está en el OpenAPI. No es la API de este producto: es el
  protocolo de otro, fijado por tráfico grabado en `compat/sentry-cli/fixtures/`
  (ADR 013).
- La tabla de rutas vive ahora en `internal/adapters/httpapi/routes.go` como
  datos: método, ruta, scope, si es pública y de qué subsistema depende. El
  router la recorre para registrar y el gate la recorre para verificar, así que
  hay una sola lista y añadir una ruta sin documentarla rompe el build.

### Añadido
- **Pantalla de rendimiento** (`/projects/{id}/performance`): la tabla de
  transactions con p50, p95, p99, volumen y porcentaje de fallo, ordenable por
  las tres preguntas que se hacen durante un incidente — qué está lento, qué
  está cargado y qué está fallando, que un gráfico de latencia no enseña nunca
  porque una petición que falla rápido parece rápida. Los percentiles se
  calculan al consultar fusionando los sketches del rango y no se guardan jamás
  (ADR 007, ADR 020), y la pantalla dice cuántas transactions llegaron, cuántas
  se guardaron enteras y con qué tasa efectiva: un número muestreado que no dice
  que lo está es un número que engaña (ADR 021).
- **La historia de una transaction**, con p50/p95/p99 sobre los mismos buckets y
  en un solo gráfico, porque lo que se lee es la distancia entre las tres
  líneas: un p95 que sube con el p50 plano es una cola, y los dos subiendo juntos
  es otro incidente distinto. Los buckets sin tráfico son huecos y no ceros —
  dibujar una noche tranquila a cero la pintaría como el mejor rato del servicio.
- **El waterfall** (`/projects/{id}/traces/{traceID}`): un span por fila,
  anidados reconstruyendo el árbol por `parent_span_id` en lugar de fiarse del
  orden en que llegaron, con la barra de proporción al lado. Se llega desde la
  transaction, que lista sus trazas guardadas más lentas.
- **Crash-free rate en las pantallas de release**: la lista lo enseña por
  release —una sola petición para toda la página, no una por fila— y el detalle
  añade las cuatro cuentas disjuntas y el aviso de que la hora en curso puede
  moverse tras un reinicio, al lado del número y no en un documento que nadie
  abre (ADR 008). Una release que no reportó sesiones no enseña nada: un cero
  se leería como «todo se cayó» y un guion como «lo comprobamos».
- `scripts/ui-smoke.sh` cubre las tres pantallas nuevas y, para la salud de
  release, **reinicia el servidor a mitad del gate**: las sesiones se cuentan en
  una ventana en memoria y se escriben al vaciarla, así que leer el crash-free
  justo después de reportarlo sería leer un número que el servidor todavía no ha
  confirmado, y esperar el temporizador de sesenta segundos metería un minuto de
  siesta en un gate de PR. De paso, la segunda mitad prueba algo que la primera
  no puede: que la sesión del panel sobrevive al reinicio, porque es un hash en
  la base de datos y no estado del proceso.

### Corregido
- El aviso de deprecación del prefijo `v1-beta` llegaba sólo a las lecturas. Al
  ponerlo por ruta quedaba **dentro** del guardia CSRF, que responde 403 por su
  cuenta a todo lo que escribe y sin cabecera, así que ningún POST, PUT ni
  DELETE lo veía: un cliente habría leído que sus escrituras estaban bien hasta
  la release que las borró. Ahora se decide por prefijo de ruta, desde fuera del
  router entero.

### Añadido

- **Monitorización de crons.** Un backup que deja de correr ahora produce un
  mensaje. Es la única función de este producto cuyo evento interesante es una
  ausencia: nadie mira un trabajo nocturno hasta que hace falta lo que ese
  trabajo producía, y ninguna cantidad de reportes de error cubre al proceso que
  no llegó a arrancar.
- **`GET|POST /ping/{clave}`**, más `/start` y `/fail`. Sin autenticación —la
  clave *es* el secreto— y con una línea de `text/plain` por respuesta.
  Instrumentar un cron de sistema es añadir `&& curl -fsS https://…/ping/abc` al
  final de la línea del crontab: ni SDK, ni token, ni fichero de configuración.
  Un ping a un monitor apagado responde **410**, no 404, porque un script que
  lleva un año usando la misma clave no tiene otra forma de enterarse de que
  alguien lo apagó.
- **El item `check_in` del envelope deja de tirarse.** El decorador de los SDKs
  oficiales (`@monitor(monitor_slug=…, monitor_config={…})`) manda el schedule
  dentro del check-in, así que **el SDK declara el monitor**: el primero lo crea
  y los siguientes lo reconfiguran si cambió. Instrumentar una tarea de una
  aplicación es una línea, igual que en Sentry. La categoría sigue siendo opt-in
  como todas: un proyecto que no la habilita recibe 429 con la contrapresión del
  protocolo y los SDKs dejan de enviarla.
- **Cinco estados —`ok`, `missed`, `timeout`, `error`, `unknown`— y cuatro
  avisos.** Un monitor recién creado es `unknown` y nunca `ok`: un semáforo en
  verde para un backup que no ha corrido jamás es la mentira más cara que puede
  contar una página de estado. `missed` es «no arrancó dentro de su margen»,
  `timeout` es «arrancó y no volvió», y el timeout gana cuando ambos son ciertos,
  porque reportar «no arrancó» un trabajo colgado manda a alguien a mirar el
  sitio equivocado.
- **Reglas de alerta `cron_missed`, `cron_timeout`, `cron_failed` y
  `cron_recovered`**, entregadas por el mismo outbox persistente que el resto
  (ADR 015): la notificación se escribe en la misma transacción que el cambio de
  estado del que habla, así que un reinicio no la pierde. La ventana de silencio
  es por monitor, de modo que un trabajo caído toda la semana avisa una vez y no
  cada treinta segundos. `cron_failed` no estaba en el plan y se añade bajo el
  ADR 036: un trabajo que corre, falla y lo reporta era el único de los cinco
  estados que no le decía nada a nadie.
- **Zona horaria por monitor, IANA.** «Las tres de la mañana» es un instante
  distinto en Caracas y en Madrid, y en media docena de países cambia dos veces
  al año. Un offset fijo (`-04:00`) se rechaza a propósito: es un hecho sobre un
  instante, y un schedule sobrevive al siguiente cambio de hora. Sin esto, el
  síntoma sería un backup reportándose como perdido sin motivo aparente.
- **Parser de crontab propio** (`internal/domain/cron.go`, sólo stdlib): cinco
  campos, `*`, listas, rangos, pasos y `@hourly|@daily|@weekly|@monthly`, con la
  regla vieja de que los dos campos de día se combinan con OR cuando los dos
  están restringidos. Lo que no acepta lo dice: un rango al revés (`22-3`)
  explica cómo escribirlo en dos ítems en vez de elegir en silencio una de las
  dos lecturas posibles.
- **Horarios por intervalo** además de crontab: `-schedule "5 minutes"`, contado
  desde el último check-in y no desde una rejilla fija, que es lo que quiere
  quien tiene un trabajo que a veces tarda más.
- **API nativa y CLI**: `GET|POST /projects/{id}/monitors/cron`,
  `GET|PUT|DELETE …/{monitorID}` y `GET …/{monitorID}/checkins`; y
  `trapline monitors cron add|list|show|update|remove|checkins`, todo con
  `--json`. El `add` imprime la línea de `curl` lista para pegar, porque
  imprimir la clave suelta dejaría a alguien montando la URL a mano.
- **Scopes `monitors:read` y `monitors:write`**, propios y no `projects:*`. Un
  monitor lleva una clave de ping, y esa clave puede reportar un backup como
  correcto desde cualquier parte de internet: no es algo que deba poder leer un
  token creado para un dashboard. Un token anterior a este build no los tiene.
- **El job `cron-watch` sólo existe si hay algún monitor habilitado** (ADR 014).
  No es un goroutine dormido comprobando una bandera: sin monitores no se
  registra, no aparece en `GET /system/jobs` y no consume nada. El primero lo
  enciende sin reiniciar, mientras quien lo creó sigue mirando la pantalla.
- **`scripts/crons.sh`** y el job `crons` en CI, con una única espera real de
  noventa segundos. Todas las transiciones de estado son función pura y se
  prueban con reloj inyectado; lo que compra la espera es lo único que un reloj
  inyectado no puede afirmar — que el scheduler, el barrido y el notificador
  están de verdad cableados entre sí en el binario que se publica. El gate
  incluye al SDK oficial de Python declarando un monitor con su decorador real.

### Añadido

- **Uptime monitors**: se le da una URL, un intervalo y lo que se espera de la
  respuesta, y el servidor la visita hasta que alguien lo pare. Método `GET` o
  `HEAD`, rango de estado esperado, una subcadena que debe aparecer en el
  cuerpo —que es lo que distingue "el balanceador contesta" de "la aplicación
  funciona": un 200 desde una página de error sigue siendo un 200—, seguimiento
  opcional de redirecciones y un `timeout` que acota el check entero, conexión
  y cuerpo incluidos. El intervalo mínimo es de **30 segundos** y es un suelo,
  no una sugerencia: cada check es tráfico que paga el dueño del objetivo.
- **Guardia SSRF propio, en `internal/ssrfguard`.** Un monitor es una URL que
  escribe un usuario para que **el servidor** la visite, y el servidor está
  dentro de la red de quien lo administra. Antes de conectar se resuelve el
  host y se rechaza si **cualquiera** de las direcciones resueltas es loopback,
  privada (RFC 1918/4193), link-local, multicast, `0.0.0.0/8`, broadcast, CGNAT
  o reservada — cualquiera, no la primera, porque un host que resuelve a una
  pública y a `127.0.0.1` puede conectar a las dos. La excepción exige **las
  dos** autorizaciones: `allow_private` en el monitor y `-uptime-allow-private`
  en la instalación, que son de dos personas distintas. El rechazo es un `422`
  con una frase que dice qué dirección era, de qué tipo y **cuál de los dos
  permisos** falta.
- **El `Dial` va contra la dirección ya validada, sin volver a resolver.** Es
  lo que hace que el guardia sirva de algo: comprobar un host y entregar
  después el nombre a `net.Dial` pregunta al DNS dos veces y conecta con la
  segunda respuesta. Las redirecciones se revalidan hop a hop, porque
  `Location` es una URL que eligió el objetivo. El paquete no importa nada
  fuera de la stdlib —frontera verificada por `internal/arch` y por depguard,
  como `clientip`— y tiene el 100 % de cobertura.
- **Estado `up`/`down` con umbral de dos fallos consecutivos para bajar y uno
  para subir.** Un fallo aislado es ruido de red, y avisar por cada uno es como
  no avisar; volver tarde de una caída es peor que volver pronto, así que la
  asimetría es deliberada. Las transiciones disparan los triggers
  `uptime_down` y `uptime_recovered` sobre el outbox de alertas, escritos en la
  **misma transacción** que registra el check: un reinicio en mitad de una
  caída no puede dejar el estado grabado y la notificación sin encolar.
- **Un job `uptime`, no uno por monitor.** Toma en lote los que están vencidos
  y los comprueba con un semáforo de 8, así que el coste es el mismo con un
  monitor que con doscientos. Se registra sólo si hay algún monitor habilitado
  y desaparece con el último (ADR 005, ADR 014).
- **Historial de checks con retención de 90 días y un agregado diario**
  (`uptime_daily`) escrito por la misma transacción que el check. Es lo que
  permitirá a la status page pintar noventa días en noventa filas en vez de escanear un
  cuarto de millón de checks, y es lo único que queda cuando la poda se lleva
  los checks: después de eso, es el único registro de que algo estuvo caído en
  marzo.
- **API nativa** (`/projects/{id}/monitors/uptime`, `/monitors/uptime/{id}` con
  resultados y agregado diario) y **CLI** `monitors uptime add|list|show|
  remove|enable|disable|results|daily`, con `--json` en todas. Sin panel
  todavía: la UI llega con la status page.
- **Scopes `monitors:read` / `monitors:write`**, propios y no `projects:*`:
  `monitors:write` es el permiso para hacer que esta instalación emita
  peticiones salientes a una dirección que elige quien llama. Un token creado
  antes de este build no los tiene.
- **Gate `scripts/uptime.sh`**, en CI en cada pull request: levanta nginx en un
  contenedor, lo apaga con `docker stop` y lo vuelve a encender, y comprueba
  que un fallo no es una caída, que dos sí, que la notificación llega firmada
  al receptor de webhooks y que la recuperación también. Y el guardia contra
  las direcciones que existe para rechazar, incluido el endpoint de metadatos
  de la nube, con un **segundo servidor arrancado sin el flag** para demostrar
  que el flag es imprescindible y no decorativo.

### Corregido

- **`ci.yml` declaraba dos jobs llamados `releases`**, y el primero corría
  `stats.sh`. Actions rechaza el fichero **entero** ante una clave duplicada,
  así que la CI llevaba sin ejecutarse desde que ese conflicto de rebase se
  resolvió mal — en silencio, y con `make workflows` en verde, porque
  `yaml.safe_load` se queda con la última de dos claves iguales y no dice nada.
  Se elimina el job sobrante y `scripts/check-workflows.py` pasa a cargar los
  workflows con un loader que **falla** ante una clave repetida: un verificador
  que normaliza lo que verifica no verifica nada.

### Cambiado

- `SECURITY.md` dejaba el guardia SSRF de los uptime checks marcado como "no
  implementado". Ya lo está, y la sección dice qué rechaza, por qué hacen falta
  dos autorizaciones y qué sigue **sin** proteger.

### Añadido

- **Página de estado pública.** `GET /status/{proyecto}` sirve una página HTML
  renderizada en el servidor —sin React, sin JavaScript de ningún tipo y con el
  CSS en línea— con el estado actual de cada monitor público, su disponibilidad
  a 24 h, 7 días y 90 días, una barra de noventa días y los incidentes. Es la
  única pantalla de este producto que lee alguien de fuera del equipo, y se lee
  precisamente cuando algo está roto: cada dependencia que no tiene es una
  manera menos de no poder decirle a esa persona lo que vino a averiguar
  (ADR 017).
- **La página se activa por proyecto y se titula por instalación.** Publicar es
  una decisión sobre *un* servicio —una instalación normal tiene uno de cara al
  cliente y varios internos— así que el interruptor vive en la configuración
  del proyecto (`config set -status-page on`, o la casilla del panel). El
  título y la descripción son de la instalación entera
  (`monitors status-page set`), porque nombran a la organización y no al
  proyecto. Un proyecto que no publica responde **404**, exactamente igual que
  un proyecto que no existe: la diferencia entre los dos es la decisión de su
  operador, y distinguirlos sería filtrarla.
- **Pantalla `Monitors` en el panel**, con las dos familias juntas: los crons
  con su URL de ping y sus últimas ejecuciones, los uptime con su barra de
  noventa días y sus últimos checks, y los ajustes de la página pública debajo.
  Una pantalla y no dos porque son un concepto mirado desde dos lados, y quien
  pregunta «¿qué está vigilando esto?» no debería tener que acordarse de cuál
  de las dos palabras se usó (ADR 037).
- **Endpoints `GET|PUT /system/settings/status-page`** y la subrama de CLI
  `monitors status-page show|set`, con `--json`. Paridad ADR 006 completa para
  todo lo que la status page añade.

### Cambiado

- **Un monitor tiene familia.** Los monitores de cron y los de uptime numeraban
  sus ids en tablas distintas, así que el cron 7 y el uptime 7 existen los dos
  y no tienen nada que ver — y ambos escribían `monitor:7` como clave de
  silencio y `/projects/2/monitors/7` como enlace. Con eso, un servicio que se
  cae **silenciaba** la alerta de un backup que no corrió. Ahora la familia
  forma parte de la identidad: clave `monitor:<familia>:<id>`, enlace
  `/projects/{id}/monitors/{familia}/{id}` y campo `monitor_kind` en el payload
  del webhook (ADR 037, en Propuesta). Un receptor que enrute por `monitor_id`
  sin mirar `monitor_kind` mezcla las dos familias; `docs/alerts/webhooks.md` lo
  dice.
- **`trapline monitors` despacha las dos familias**: `monitors cron …`,
  `monitors uptime …` y `monitors status-page …` cuelgan del mismo verbo. Las
  dos ramas se desarrollaron en paralelo con un despachador cada una, y la
  segunda habría tapado a la primera sin que Go dijese nada.
- **La barra de noventa días distingue «no había monitor» de «no corrió nada».**
  Un monitor creado esta mañana no dibuja ochenta y nueve días verdes detrás:
  eso sería una afirmación de tres meses de disponibilidad que ningún dato
  respalda. Y una ventana sin checks muestra «—», no «100 %».

### Corregido

- **La página nunca redondea hacia arriba hasta el 100 %.** 99,996 % se imprime
  como `99.99 %`: decirle «100.00 %» a quien acaba de aguantar una caída es
  decirle que no pasó.

### Añadido
- **El issue bundle: un error entero en una sola lectura, en markdown.** `GET
  /projects/{id}/issues/{issueID}/bundle` devuelve el stacktrace symbolicado con
  el código alrededor de cada frame y la línea minificada de la que salió, los
  breadcrumbs, los tags y contextos agregados, el ciclo de vida de release, con
  qué frecuencia está pasando en las últimas 24 horas y en la última quincena, y
  los commits que probablemente lo causaron. Antes eran cinco llamadas y cinco
  JSON; ahora es una petición y un documento. Las cifras de frecuencia salen de
  los agregados horarios, así que siguen respondiendo después de que los eventos
  hayan caducado.
- El mismo documento por los otros tres clientes: `trapline issues bundle
  -project <id> -issue <id>` lo escribe tal cual en stdout —listo para
  `> issue.md` o para un pipe—, el detalle del issue en el panel tiene un botón
  **«Copy for an agent»**, y la herramienta MCP `get_issue_bundle` devuelve los
  mismos bytes. El gate compara los cuatro.
- **`trapline mcp`: el producto como servidor MCP, por stdio.** Un agente que
  corre en la misma máquina lo lanza como subproceso y ya tiene diez
  herramientas: `list_projects`, `list_issues`, `get_issue`, `get_issue_bundle`,
  `resolve_issue`, `ignore_issue`, `reopen_issue`, `get_release_health`,
  `query_stats` y `list_transactions`. Necesita `TRAPLINE_URL` y
  `TRAPLINE_TOKEN` y nada más; no abre la base de datos, así que sirve igual de
  bien a una instalación que está en otro sitio.
- **`POST /mcp`: las mismas herramientas para un agente que no está en esta
  máquina.** Autenticado con `Authorization: Bearer`, como el resto de la API.
  Es la misma tabla que sirve `trapline mcp`, y el gate exige que `tools/list`
  sea idéntico por los dos transportes.
- Las herramientas aceptan el **slug** de un proyecto donde la API pide un id,
  así que un agente que acaba de leer `list_projects` puede devolver el nombre
  que vio en vez de tener que extraer un número del JSON.
- `make mcp` corre el gate nuevo: levanta un servidor, habla los dos
  transportes con un cliente MCP real, compara los bundles contra los de REST y
  termina comprobando que un token de sólo lectura no puede resolver un issue.

### Cambiado
- Un token sigue necesitando `projects:read` para conectarse por MCP y
  `projects:write` para las tres herramientas que cambian algo. No hay scopes
  nuevos: una herramienta MCP es una llamada a la API con la credencial de quien
  pregunta, así que la permite o la rechaza exactamente el mismo guardia que
  sobre REST. Un rechazo llega como error de herramienta con el estado HTTP en
  el texto, no como error de protocolo — un agente puede reintentar con otro
  token en lugar de abandonar el turno.

### Corregido
- **Una avalancha de bombas de descompresión podía llevar la memoria del
  proceso a 1,46 GB, cuarenta y ocho veces la huella publicada, con 151 KB de
  subida en total.** Los topes por petición estaban puestos y eran correctos
  —20 MiB por envelope, 4 MiB por item, cien items, y un lector acotado que
  corta la expansión de gzip y zstd—; lo que no existía era un techo a cuántas
  peticiones podían sostener esos topes **a la vez**, y en zstd cada una además
  reservaba una ventana del tamaño que la propia trama declarase. Ahora la
  ingesta comparte un presupuesto de 32 MiB: cada petición reserva por
  adelantado el working set de su descompresor y va pagando los bytes que
  produce, y cuando el presupuesto se acaba la siguiente recibe `429` con
  `Retry-After` en vez de memoria. El pico bajo el mismo ataque es de 126 MB y
  **no crece con la avalancha**: cuadruplicarla lo mueve un 10 %, no un 400 %.
  Un envelope de 20 MiB, que es el máximo documentado, se sigue aceptando
  (ADR 039).
- **Una bomba de descompresión respondía `500` en vez de `413`.** El centinela
  que significa «el cliente envió de más» sólo se traducía en la vía que lee
  una línea de cabecera, no en la que lee un payload con longitud declarada —
  que es la que toman todas las bombas y todos los SDK reales. Las
  consecuencias no eran cosméticas: un SDK lee `5xx` como culpa del servidor y
  reintenta para siempre, cada intento escribía una línea de `ERROR`, y
  `doctor` mostraba errores internos de algo que este servidor no había hecho
  mal. Ahora responde `413`, que es terminal, y no ensucia el log.
- Un frame zstd podía declarar una ventana de hasta 20 MiB y hacer que el
  servidor la reservara antes de producir un solo byte. La ventana se acota a
  8 MiB, que es la mayor que produce cualquier compresor corriente.

### Añadido
- **`docs/benchmarks/footprint.md`: lo que esto cuesta de verdad, medido y
  reproducible.** Dos perfiles —una instalación mínima y otra con absolutamente
  todo encendido: errores, agregados, búsqueda, source maps, tracing, release
  health, alertas, diez monitores, página de estado y digest— y cada uno medido
  de tres maneras: en reposo, en pico ingiriendo y en pico siendo leído. Con la
  comparativa honesta frente a los requisitos que publica el incumbente.
- `make footprint` y `make hardening`, los dos gates nuevos, en `nightly.yml`.
  El primero mide y hace fallar el build si alguna de las cifras publicadas
  deja de ser cierta. El segundo es el pase de seguridad entero como algo que
  corre: techos por dirección con y sin proxy de confianza, bombas de gzip y
  zstd a dos concurrencias, cabeceras en panel, API, página de estado, ping y
  404, el header CSRF exigido donde hay cookie y en ningún otro sitio, tokens
  caducados, y `gosec` sin hallazgos altos.
- Fuzzing de diez minutos, cada noche, de los cuatro parsers que leen bytes que
  eligió otro: el framing de envelopes, el payload de evento (objetivo nuevo,
  `FuzzDecodeEvent`), un source map subido por un usuario y un sketch de
  latencias leído de disco.

### Cambiado
- `SECURITY.md` decía que un atacante distribuido «no puede agotar la memoria».
  Era falso para la ingesta hasta este cambio; ahora lo dice nombrando los dos
  topes globales que lo hacen cierto, y dice cuál era la cifra cuando no lo era.

### Añadido

- **Una skill lista para usar, `/fix-error`** (`skills/fix-error/SKILL.md`): el
  procedimiento completo para que un agente arregle un error de producción —
  leer el issue bundle antes de abrir ningún fichero, localizar el código por
  los frames, escribir el test que falla, parchear, correr tu suite y marcar el
  issue resuelto para la próxima release. Con límites duros: nunca hace `push`,
  nunca despliega, y nunca resuelve un issue cuyo test no pasa.
- **Documentación de usuario**: instalar (`docs/install.md`) de cero a un primer
  error visible, migrar desde Sentry (`docs/migrate-from-sentry.md`) con la
  matriz de SDKs probados y la lista de lo que no existe y no va a existir, y
  operar desde un agente (`docs/agents/operating.md`) con la CLI y con MCP,
  incluidos los seis scopes y los códigos de salida.
- **Índice para agentes** (`docs/agents/llms.txt`) y **las decisiones en orden
  de lectura** (`docs/architecture/README.md`), que agrupa los ADRs por la
  pregunta que responden en vez de por cuándo se tomaron.
- **`README.md` reescrito** con la matriz de compatibilidad y las cifras de
  huella medidas (19,7 MB en reposo en el perfil mínimo, 34,2 MB con todo
  encendido, binario de 18,13 MB), cada una enlazada a la metodología que la
  produce.
- **`make docs`**: la documentación como gate. Cada bloque marcado `sh test` se
  ejecuta contra un servidor de verdad, y cada enlace interno y cada ancla
  tienen que resolver. Corre en cada PR.

### Corregido

- **`trapline issues resolve -h` decía «Usage of issues resolved»** —y `reopen`,
  «Usage of issues unresolved»—, nombrando comandos que no existen justo en el
  sitio donde alguien mira porque ya se equivocó al invocar. La línea de uso se
  derivaba del estado que se le manda a la API en vez del verbo que se teclea.
- **La documentación decía que había dos scopes y hay seis.** `alerts:read`,
  `alerts:write`, `monitors:read` y `monitors:write` existen y son distintos de
  `projects:*` por razones concretas: un canal de alerta guarda credenciales y
  un monitor cron guarda una clave de ping.

### Añadido
- **La demo**, en `demo/`: dos contenedores —el servidor y un servicio de
  checkout con un bug de verdad— y un script que recorre la vuelta entera.
  Llega el error, el bundle lo explica y señala el commit que lo introdujo, el
  parche entra con su test, el issue se cierra para la próxima release, se
  redespliega y el error no vuelve — mientras la instancia que nadie ha
  redesplegado sigue lanzándolo, contada y sin reabrir nada. `make demo` la
  corre con el paso del agente sustituido por sus parches; cada noche también.

### Corregido
- **`releases commits -repo` sin `-from` hacía sospechoso al commit
  equivocado.** Sin rango, la subida incluye el import inicial, que creó todos
  los ficheros del stacktrace y por eso gana la puntuación al commit que
  realmente rompió algo. No es un fallo del código: la atribución es tan
  estrecha como el rango que se le da (ADR 019). La ayuda del comando y
  `docs/agents/operating.md` ahora lo dicen, y la demo pasa el `-from` de la
  release anterior como haría un pipeline.
