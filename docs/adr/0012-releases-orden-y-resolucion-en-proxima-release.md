# 012 — Releases, orden entre releases y resolución "en la próxima release"

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El producto ya sabía decir "este error volvió". Lo decía mal.

La regla era: cualquier evento sobre un issue resuelto lo reabre. Esa regla
falla en la situación más común que existe. Alguien arregla un bug, marca el
issue como resuelto, despliega — y durante el rollout las instancias que
todavía corren el build viejo siguen emitiendo el mismo error. El issue se
reabre segundos después de haberse resuelto, con un evento que no prueba nada
salvo que un pod aún no se reinició.

Quien ve eso dos veces deja de creerle al estado. Y un estado en el que nadie
cree es peor que no tener estado: ocupa espacio en la pantalla, genera
notificaciones y no informa. La señal de regresión es lo más valioso que
produce un tracker de errores; un falso positivo estructural la destruye.

Para arreglarlo hace falta responder una pregunta que el esquema no podía
responder: **¿este evento viene de una release posterior a aquella en la que
se declaró arreglado?** Eso exige que las releases existan como entidad, y que
haya un orden entre ellas.

El orden es el problema difícil. La mayoría de los identificadores de release
reales no son versiones: un sha de git, un número de build de CI, una marca de
fecha. Nada en la cadena `a3f9c1e` dice si vino antes o después que `b7d2f04`.

## Decisión

### 1. Releases como entidad

`releases(id, project_id, version UNIQUE por proyecto, created_at,
date_released, first_event_at, last_event_at, commit_count)`,
`release_commits(release_id, sha, message, author_name, author_email,
timestamp, repository, ordinal)`, `release_commit_files(release_id, sha, path,
change_type)` y `deploys(release_id, environment, name, url, started_at,
finished_at)`.

**Alta implícita**: un evento con una `release` desconocida **crea** la
release, con su `first_event_at`. La CLI propia y `sentry-cli` la crean
explícitamente. La implícita no es un caso degradado sino el normal: la
mayoría de las instalaciones nunca corren una herramienta de despliegue, y un
producto que sólo conociera las releases que alguien se acordó de registrar no
conocería casi ninguna.

`release_commit_files` guarda las rutas, no sólo los shas, porque son la
entrada entera de los *suspect commits*: la intersección entre los
ficheros de un stacktrace y los que una release tocó. Registrarlas cuando
llegan los commits convierte esa respuesta en una consulta, en vez de en un
clon del repositorio en el momento en que alguien intenta leer un error.

### 2. El orden entre releases es una función pura

`CompareReleases(a, b string, order ReleaseOrder) int` en `internal/domain`,
con `ReleaseOrder` un mapa `versión → rango de primera aparición` que le pasa
quien la llama. Las reglas, en el orden en que aplican:

1. **Una release ausente nunca es más nueva que una nombrada.** Un evento sin
   release no dice nada sobre de qué despliegue viene, y adivinar "más nueva"
   reabriría el issue sin evidencia — justo el falso positivo que esto viene a
   eliminar.
2. **Si ambas parsean como semver y son del mismo paquete, orden semver**
   (formato `pkg@1.2.3` o `1.2.3`, con pre-release; los metadatos de build se
   ignoran, semver §10). Es la única rama capaz de ordenar una release que el
   servidor nunca ha visto, que es lo que permite entender un despliegue antes
   de que llegue su primer error.
3. **Si no, orden de primera aparición.** El `id` autoincremental de
   `releases` **es** ese orden: se asigna cuando la release se ve por primera
   vez. No hay una columna extra ni un contador aparte.
4. **Una release nunca vista, frente a una vista, es más nueva.** Algo la está
   emitiendo ahora y no la emitía antes. Es evidencia débil, pero apunta en
   una dirección; tratarla como más vieja tragaría en silencio la regresión
   que introdujo un build recién salido.

Dos releases distintas que nadie ha visto devuelven `0`: no hay información
con la que ordenarlas, e inventar una respuesta es peor que admitirlo.

**Por qué exactamente tres componentes numéricos y no dos.** Leer `1.2` como
`1.2.0`, o `2026.08.29-nightly` como una versión, metería identificadores que
no son versiones en la rama semver de la comparación. Equivocarse sobre cuál
release es más nueva es precisamente el fallo que esta ADR existe para
prevenir, y el fallback por primera aparición nunca está *mal* — sólo informa
menos.

**Por qué el paquete importa.** `api@2.0.0` no es más nuevo que `web@3.1.0`:
son cosas distintas que ambas cuentan hacia arriba. Con paquetes distintos se
cae al orden de primera aparición.

### 3. Resolución "en la próxima release"

`POST /issues/{id}/status` acepta `{status:"resolved", in_next_release:true}`.
Columnas nuevas en `issues`: `resolved_at`, `resolved_in_release`,
`resolve_next_release`, `first_release`, `regressions`,
`regressed_in_release` y `seen_in_resolved_release_count`.

La regla vive en `Issue.Observe`, en el dominio:

- **Modo normal** → cualquier evento reabre. Se conserva intacto: quien
  resolvió sin nombrar una release está afirmando que está arreglado *ahora*,
  no que se arregla pronto.
- **Modo `next_release`** → reabre **sólo** si
  `CompareReleases(evento.release, resolved_in_release) > 0`. Si es igual o
  anterior, el evento **se cuenta** (`times++`), **se almacena** y
  **no reabre**; incrementa `seen_in_resolved_release_count`, que la API
  expone para que "suprimimos 400 de estos" sea visible en vez de parecer que
  el producto los perdió.
- Reabrir incrementa `regressions` y registra `regressed_in_release`.
- Reabrir a mano **no** cuenta como regresión: una regresión es el producto
  diciendo que algo volvió; un reopen manual es una persona diciendo que se
  equivocó. Contarlo corrompería el único número que significa "esto pasa una
  y otra vez".

### 4. Dónde se escriben esas columnas

Están en `issues`, pero **no** las escribe `RecordEvent`. Las escribe
`releases_repository.go` / `issue_resolution.go` en sentencias aparte, y el
caso de uso de ingesta hace **una** llamada después de registrar el evento.

La razón es de coste de cambio, no de rendimiento: `RecordEvent` es la
escritura más caliente del producto y aquella de la que toda funcionalidad
futura quiere un trozo. Manteniendo el ciclo de vida de releases fuera de esa
transacción, se puede construir, cambiar y revertir sin tocarla. El precio es
una sentencia extra en los dos caminos raros — un issue creándose y un issue
reabriéndose — y ninguna en el común.

## Consecuencias

- **`Issue.Observe` cambia de significado, no de firma.** `Observation` gana
  un campo opcional `ReleaseOrder`. Sin él, la comparación sigue ordenando
  cualquier cosa que parsee como semver y trata el resto como
  indistinguible — degradación honesta, no fallo.
- **`events` en el detalle de una release es `*int64` y se omite cuando no se
  sabe.** El número vive en los agregados horarios (ADR 010); mientras esa
  tabla no exista, la respuesta honesta es la ausencia del campo. Un cero
  sería mentira y contar filas de `events` sería el escaneo que la ADR 001
  prohíbe — y dejaría de ser cierto en cuanto la retención borrase esos
  eventos.
- **`regressed_in_release` es una columna y no se deriva de `last_release`.**
  `last_release` sigue moviéndose con cada evento posterior: un issue que
  volvió en 1.0.1 y siguió fallando hasta 1.0.3 le habría echado la culpa a
  1.0.3, que no desplegó nada que ver. Sólo se recuerda la regresión más
  reciente; el historial completo de regresiones sería una tabla, y no hay
  todavía una pregunta que la pida.
- **`seen_in_resolved_release_count` sobrevive a la regresión** y se reinicia
  al resolver de nuevo. "Llegaron tres del build viejo antes de volver de
  verdad" es exactamente lo que quiere saber quien lee una regresión.
- Un issue nacido antes de esta migración tiene `first_release` vacío hasta
  que llega un evento que nombre una release. No se rellena hacia atrás: no
  hay forma de saberlo sin escanear `events`.
- La versión es un segmento de path en todas las rutas que la nombran, así que
  una versión con `/` se rechaza al crearla. Una que se pudiera crear y nunca
  volver a leer sería peor.
- Los endpoints nuevos usan `projects:read|write` en vez de un par de scopes
  propio. Los scopes de v1 son gruesos a propósito (`domain/token.go`); un
  scope nuevo dejaría fuera a todo token acuñado antes de este build —
  incluido el del pipeline de despliegue, que es justo el llamante para el que
  existen estas rutas.

## Medición (2026-08-29)

El coste de la ingesta con el lado de releases cableado, en el mismo banco y
la misma máquina que el ADR 001 (i7-14650HX, WSL2, evento realista de 14,6 KB,
5 repeticiones de 2000 eventos):

| | sin releases | con releases | coste |
|---|---|---|---|
| issue nuevo (INSERT) | 640 ev/s mediana | 577 ev/s mediana | **−10%** |
| issue existente (UPDATE) | 753 ev/s mediana | 646 ev/s mediana | **−14%** |

El gate son 150 ev/s y sigue cumpliéndose por **4,3×**.

Los ~150-220 µs por evento son casi enteros el commit de la transacción
implícita del `UPSERT` sobre `releases`, no el upsert en sí: bajo WAL con
`synchronous = NORMAL` lo que se paga es el commit, y este es un commit más
por evento. El banco se cableó a propósito con el lado de releases activo —
medir un ensamblado que nadie despliega no es medir nada.

**La salida está identificada y no se toma aquí**: plegar el `Ensure` dentro
de la transacción de `RecordEvent` elimina ese commit, exactamente como el
ADR 010 hace con los buckets horarios. No se hace en este trabajo porque
`RecordEvent` pertenece a otra unidad en vuelo, y tocar la escritura más
caliente del producto desde dos sitios a la vez es cómo se pierden eventos.
Es trabajo de un paso de integración posterior, con el banco como juez.

## Revisado por

El [ADR 032](0032-la-regla-de-reapertura-en-la-transaccion-de-ingesta.md)
supersede el punto 4 de la decisión de arriba —las columnas de resolución las
escribe ahora `RecordEvent`, en la misma transacción que el evento— y la
consecuencia sobre `events`, que deja de ser opcional porque los agregados
horarios del ADR 010 ya son parte del esquema de toda instalación. Lo demás
sigue vigente tal cual: el orden entre releases, la regla de reapertura y
dónde vive.
