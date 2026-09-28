# 021 — Tracing: sampling determinista por trace, agregados por minuto con downsampling

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El item `transaction` del envelope llegaba desde el primer día y se contaba como
`not_yet_stored`. Este ADR es el que decide qué se hace con él.

Un transaction no es un evento. Un evento es algo que salió mal y merece
guardarse entero; un transaction es **una muestra de una distribución**, y lo
que importa de él es su duración, su nombre y si falló. Esa diferencia decide
casi todo lo que hay aquí.

Hay tres tensiones, y ninguna se resuelve sola:

1. **Volumen.** Un servicio con tráfico manda órdenes de magnitud más
   transactions que errores. Guardarlos todos enteros es una promesa de huella
   rota antes de la primera semana.
2. **Los percentiles no son componibles** (ADR 007, ADR 020). Lo que se guarde
   por ventana tiene que poder fusionarse.
3. **Un waterfall a medias es peor que ninguno.** Si el sampling se decide por
   transaction, una petición que cruza tres servicios se guarda a trozos, y los
   huecos resultantes se parecen exactamente a instrumentación que nadie
   añadió.

## Decisión

### Sampling determinista por trace, y sólo sobre el raw

- Knob por proyecto `config.traces_sample_rate` (0–1, **default 0,1**).
- La decisión es `hash(trace_id) < rate`, en el dominio
  (`domain.SampleTrace`), determinista **por trace y nunca por transaction**:
  todas las transactions de un mismo trace llegan al mismo veredicto, en este
  proceso y en el siguiente, sin estado compartido. Un waterfall se guarda
  entero o no se guarda.
- El hash es FNV-1a de 64 bits **seguido del finalizador `fmix64` de
  MurmurHash3**. El mezclado no es opcional y no estaba en el plan: FNV-1a
  pliega cada byte en el extremo bajo del acumulador, así que sus bits altos
  los dominan los primeros bytes vistos. Con trace ids que comparten prefijo
  —un fixture, un generador secuencial, una librería con epoch fijo— tomar los
  53 bits altos de la suma cruda dejaba a los cuatrocientos del mismo lado del
  umbral: el sampling *parecía* funcionar a escala y no guardaba absolutamente
  nada para toda una clase de entrada. Lo cazó la suite HTTP; el test
  estadístico de veinte mil ids no lo veía.
- No es una decisión criptográfica: lo peor que consigue un adversario
  eligiendo trace ids es que se guarden sus propios traces, que ya consigue
  mandando más.
- **Si el SDK ya sampleó**, se acepta lo que llega y el knob del servidor se
  aplica **encima**: las tasas componen. El rate del SDK se lee del objeto
  `trace` de la cabecera del envelope y, si no está, de la cabecera HTTP
  `baggage` — las dos, porque leer sólo una haría que la tasa efectiva fuera
  correcta para unos clientes y no para otros, con nada que dijera cuáles.
- **La tasa efectiva se muestra**, y se muestra *observada* y no supuesta: la
  respuesta del listado lleva `sampling{server_rate, received, stored,
  effective_rate}`, donde `stored/received` sale de la columna `sampled` de las
  propias ventanas. Un panel que dijera «12 traces» sin decir qué fracción del
  tráfico son es un panel que alguien compara con el contador de su balanceador
  y del que concluye que el producto pierde datos.

### Los agregados cubren el 100 % de lo recibido

Esto es lo central. El agregado se escribe **para toda transaction que llega**,
y sólo después se consulta el sampling, que decide **una sola cosa**: si se
guarda el raw de los spans. Un diseño que sampleara primero convertiría el p95
en un percentil de la décima parte del tráfico — un número distinto, con el
mismo aspecto.

### Tablas

- `txn_minute(project_id, txn_name, minute, count, failed, sampled, sketch)`,
  retención **48 h**.
- `txn_hour(project_id, txn_name, hour, count, failed, sampled, sketch)`,
  retención `retention_days.transaction` (**7 d** por defecto).
- `traces(trace_id, project_id, txn_name, timestamp, duration_ms, status, op,
  spans BLOB zstd)`, sólo los muestreados, misma retención que `txn_hour`.
- `failed` = estado de la transaction **fuera de** `{ok, cancelled, unknown}`.
  Se lista lo que *no* es fallo, porque la lista de fallos es abierta: un
  estado que este build no conoce es mucho más probable que sea una forma nueva
  de fallar que una forma nueva de acertar, y al revés la tasa de fallo
  sub-reporta — la dirección que nadie nota. Un estado vacío **no** es fallo:
  varios SDKs lo omiten para una transaction que fue bien.
- La columna se llama `txn_name` y no `transaction`: TRANSACTION es palabra
  reservada en SQLite, y un identificador entrecomillado que SQLite no reconoce
  es **silenciosamente un literal de cadena**, así que una errata haría que
  todas las filas reportaran el mismo nombre inventado en vez de fallar. La API
  y la CLI siguen diciendo `transaction`.

### Downsampling

Job `downsample` del scheduler (intervalo 10 min), **registrado sólo si algún
proyecto acepta transactions** (ADR 014) — y la condición pregunta a la
*configuración*, no a las tablas, para que un proyecto encendido hace un minuto
tenga job antes de su primera transaction y no después.

Funde los minutos cerrados hace **más de 2 h** en su hora y los borra. El
margen es el punto: un minuto sólo es seguro de plegar cuando ya no puede caer
nada más en él, y todavía puede — un SDK que bufferiza, un cliente móvil que
reporta una sesión que grabó sin red, una cola atascada. Plegar al filo de la
hora perdería lo que llegó tarde, y la pérdida sería invisible.

La fusión es **exacta** (ADR 020), así que una hora construida plegando sesenta
minutos es la hora que esas transactions habrían producido si se hubieran
grabado directamente en ella. El gate lo comprueba: los tres percentiles salen
idénticos bit a bit.

`errtrack downsample -age <duración>` corre un pase a mano. `-age 0` pliega
todo minuto cerrado, que es «el reloj adelantado» dicho como una condición
sobre qué buckets son elegibles en vez de sustituyendo el tiempo — sustituirlo
sería probar el sustituto.

### Percentiles al consultar

Una consulta lee **las dos tablas** del rango y las fusiona. No es una
optimización, es la única respuesta correcta: las últimas dos horas viven en
`txn_minute` y todo lo anterior en `txn_hour`, así que un gráfico que leyera
una sola acabaría con dos horas vacías al final o con un muro de ellas al
principio, según cuál. Como el pliegue **borra** el minuto, no hay doble conteo.

**Nunca se persiste un percentil.**

## Consecuencias

- `usecase/ingest.go` deja de contar `not_yet_stored:transaction`. Un item
  malformado se cuenta como `invalid_transaction` y **no cuesta el resto del
  envelope**.
- Los `data` de los spans se **scrubean antes de escribir**, con el mismo
  scrubber de la ingesta: ahí es donde un driver de base de datos pone la
  sentencia que ejecutó y un cliente HTTP las cabeceras que mandó, así que
  llevan credenciales con la misma probabilidad que el `request` de un evento
  (SECURITY.md).
- El waterfall se guarda **normalizado**, no verbatim: la forma de la API no
  depende de qué SDK produjo el trace, y los bytes que llegan al disco han
  pasado por el scrubber.
- Un trace con transactions de varios servicios acumula sus spans en una fila,
  con las columnas de cabecera describiendo la transaction que empezó antes
  —la raíz— y un presupuesto de 256 KiB sin comprimir para que una petición
  patológica no produzca una fila de megabytes.
- La cardinalidad de `txn_name` la elige el SDK. Se acota cada fila a 200
  caracteres; acotar el *número* de nombres es problema del operador y del
  panel, no de una truncación.
- Un sketch corrupto en una ventana no cuesta ni la transaction (en escritura
  se empieza uno nuevo y la cuenta sigue siendo honesta) ni el listado (en
  lectura se salta esa ventana y se registra un aviso). La alternativa —
  rechazar toda transaction de un endpoint con tráfico, o dejar sin respuesta
  la página que alguien abrió durante un incidente— es peor.
- **Sessions no están aquí.** El ADR 008 y la mitad de sessions de este tema
  van en release health, anexado más abajo.

## Medición

`scripts/tracing.sh`, con el SDK oficial de Go y 500 transactions de latencias
log-normales cuyos percentiles exactos calcula el emisor antes de serializar:

| | exacto | servidor | desviación |
|---|---|---|---|
| p50 | 20,976 ms | 21,116 ms | 0,67 % |
| p95 | 85,830 ms | 85,635 ms | 0,23 % |
| p99 | 151,698 ms | 152,951 ms | 0,83 % |

Con `traces_sample_rate=0.1`: 500 recibidas, 500 agregadas, **56 traces
guardados** (11,2 %). Tras `downsample -age 0` (18 minutos plegados): los tres
percentiles **idénticos** a los de antes del pliegue.

Throughput de ingesta de transactions (`make bench`, mismo umbral de 150/s que
los errores): **1884/s** en la máquina de referencia, contra 586–648/s de los
dos workloads de errores. Es más rápido porque no hay grouping, ni fingerprint,
ni índice FTS, ni upsert de tags — sólo el read-modify-write del sketch y, una
vez de cada diez, un waterfall comprimido.

## Anexo: la mitad de sessions (2026-08-29)

La sección «Sessions» de la decisión queda ejecutada. Lo estructural está en el
anexo del ADR 008 —la ventana, el LRU, el TTL, el drain, la imprecisión que se
acepta— y aquí queda sólo lo que es de este tema: el almacenamiento y la
superficie.

- **Tabla `session_hourly(project_id, release, environment, hour, started,
  errored, crashed, abnormal)`**, migración **0026**, `STRICT`, PK compuesta e
  índice `(project_id, hour)` para la consulta del panel, que es «todas las
  releases de este proyecto en este rango» y no puede usar la PK.
- **Dos items, un camino.** `session` (un update de una session) y `sessions`
  (agregados que el SDK ya sumó) entran por `usecase.Health.Accept` y terminan
  en los mismos cuatro contadores. El agregado **no pasa por la ventana**: no
  trae ids, así que no hay nada que deduplicar ni final que esperar.
- **La hora es la de inicio de la session**, no la del update que la cerró. Es
  lo que hace que «la release de las 14:00 salió mala» sea una afirmación sobre
  la release y no sobre cuándo la gente cerró el portátil.
- **Una session sin release se rechaza** y se cuenta en `Dropped`. No hay
  pregunta que pueda responder: release health es lo único que esta tabla
  contesta.
- **`environment` se guarda tal cual llega**, vacío incluido. Renombrarlo a
  «production» sería este servidor inventando un hecho sobre el despliegue de
  otro.
- **`crash_free_rate` nunca se guarda**, igual que un percentil no se guarda
  (ADR 007): se calcula sumando el rango. Y es `null`, no 1, cuando no hubo
  sessions — decir «100 % correcto» cuando la verdad es «nadie ha reportado»
  esconde la más alarmante de las dos.
- **Job `session-flush`** en el scheduler, registrado sólo si algún proyecto
  acepta la categoría `session` (ADR 005, ADR 014). La condición pregunta a la
  configuración, no a la tabla, para que encender sessions traiga el job antes
  de la primera session y no después.
- **Retención** en la ventana de `aggregates` (400 días), no en la de la
  categoría: estas filas *son* el resumen que sobrevive a lo que lo produjo, y
  no hay payloads de session que caduquen antes porque no hay filas de session.
- **API**: `GET /projects/{id}/releases/{version}/health?from&to` y
  `GET /projects/{id}/health`. **CLI**: `errtrack releases health`. **UI**: llega
  con la congelación de `/api/v1/`.
