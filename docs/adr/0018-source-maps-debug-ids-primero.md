# 018 — Source maps: debug ids primero, release + URL después

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Un error de front-end llega minificado. El frame dice `bundle.min.js`, línea 3,
columna 489, y la función se llama `t`. Sin resolverlo, el producto entero está
enseñando el sitio equivocado: agrupa por un nombre que cambia con cada build y
señala una línea que no existe en ningún repositorio. Eso es lo que source
maps resuelve.

Este ADR se escribió como hipótesis antes de que nadie hubiera visto el
protocolo. Después se grabó el tráfico real de `sentry-cli` contra Sentry, y
**la grabación manda sobre la hipótesis**: donde discrepan, este documento
dice lo grabado y anota qué cambió. La grabación dejó nueve puntos; los que
tocan a la lectura de mapas están resueltos aquí y los que tocan al protocolo
de subida se resuelven en el anexo de la superficie de subida, más abajo.

Tres hechos de la grabación ordenan todo lo demás:

1. **El camino por defecto de la herramienta es el debug id, no la release.**
   `sentry-cli sourcemaps upload dist` sin más argumentos sube un artifact
   bundle y no menciona una release en ninguna parte (grabado). Un servidor
   que sólo supiera resolver por `release + url` no resolvería nada de lo que
   sube un pipeline moderno.
2. **`sourcemaps inject` mueve el código y corrige el mapa.** Inserta dos
   líneas al principio del script y antepone dos `;` a `mappings` para
   compensar (grabado). El mapa que hay que guardar es el corregido, y un
   parser probado contra un mapa pre-inyección resolvería todo desplazado dos
   líneas sin que nada fallara.
3. **El evento trae imágenes para unos ficheros y no para otros.** El script de
   la página no se inyectó; el bundle sí. Resolver unos y dejar los otros como
   están tiene que ser el comportamiento normal, no un caso de error.

## Decisión

### Dos vías de resolución, en este orden

1. **Debug id**: `debug_meta.images[type=sourcemap]` del evento trae `code_file`
   (la URL que cargó el navegador) y `debug_id`. La unión es
   `frame.abs_path` — o `filename` — contra `code_file`, y de ahí al artefacto
   con ese `debug_id` **y `kind = source_map`**: el script y su mapa comparten
   el mismo id, así que el `kind` es lo único que los distingue (grabado).
2. **Legacy**: `release` + `dist` + la URL del frame en su forma `~/ruta`. Se
   consulta sólo si la primera vía no encontró nada, y necesita **dos**
   búsquedas: la URL nombra el *script*, y el mapa que hay al lado lo nombra la
   cabecera `sourcemap` del manifiesto — **por nombre de fichero, no por URL**.
   Adivinar `nombre + ".map"` acierta lo bastante a menudo como para parecer que
   funciona.

El orden no es una preferencia: es lo que hace la herramienta. Y la primera vía
gana siempre que responda, porque un debug id nombra el build exacto que corrió
mientras que una URL sólo nombra una ruta bajo la que alguien subió algo.

### Resolución en ingesta, nunca al leer

El frame se reescribe **después de decodificar y antes de agrupar y de
limpiar**. Las tres posiciones son deliberadas:

- **Antes de agrupar**, porque el fingerprint tiene que calcularse sobre el
  frame que una persona va a ver. Resolver después significaría que el panel
  enseña `checkout.js` mientras la identidad se tomó de `bundle.min.js`, y
  entonces el mismo bug reconstruido con otro minificador sería otro issue —
  que es exactamente el fallo que los source maps existen para quitar.
- **Antes de limpiar**, porque el scrubber es lo último que toca el payload
  antes de escribirlo (SECURITY.md). Un mapa lleva código original embebido, y
  una `context_line` sacada de ahí pasa por el scrubber como todo lo demás en
  vez de rodearlo.
- **No al leer**, porque resolver al abrir un issue significaría resolver los
  mismos frames en cada visita, que borrar un artefacto des-resuelva en
  silencio un issue que alguien ya había leído, y —lo peor— que grouping y
  panel discrepen para siempre.

**El frame original se conserva en `frame.raw`** (fichero, función, línea y
columna minificadas). Las dos mitades responden preguntas distintas: la
resuelta dice dónde está el bug, y la cruda es lo único que se puede comparar
contra el bundle desplegado cuando alguien sospecha que el mapa está mal o
viejo. Sin ella, una subida equivocada es indistinguible de una línea
equivocada.

### Parser propio, puro y fuzzeado

`internal/sourcemap`, sólo stdlib, sin importar nada del repo — verificado por
el test de arquitectura y por `depguard`, como `internal/envelope`. Sólo v3;
`sections` soportado (se aplana al parsear, con el offset de columna aplicado
**sólo a la primera línea generada** de cada sección); `x_facebook_*` y el
resto del espacio de nombres de vendor **ignorados**.

Ignorar es una decisión, no una omisión: `x_facebook_offsets` cambia lo que
significa un número de línea en el mundo de Metro, y un parser que leyera la
clave sin implementar su semántica resolvería cada frame de un bundle de React
Native a una línea segura y equivocada. Ignorándola se da la respuesta de v3
plano, que es al menos la que da cualquier otra herramienta.

El parser lee ficheros que sube un usuario, así que tiene techos y `FuzzSourceMap`:
64 MB por documento, 2²¹ segmentos, 2²⁴ líneas generadas, 4096 secciones, 6
dígitos por valor VLQ y comprobación de desbordamiento **antes** de desplazar.
Un valor VLQ que desborda un `int32` no da un mapping que falta: da un mapping
que apunta a un sitio real y equivocado, que nadie aguas abajo puede detectar.

### Caché LRU acotada por bytes

`sourcemap.Cache`, **≤ 64 MB por defecto**, configurable con
`-sourcemap-cache-mb` / `ERRTRACK_SOURCEMAP_CACHE_MB`.

Acotada por **bytes y no por número de mapas**, porque el tamaño de un mapa
parseado lo elige quien sube: un chunk de vendor son decenas de kilobytes y un
bundle de aplicación con `sourcesContent` son decenas de megabytes. Una caché
de «200 mapas» es una caché cuyo consumo de memoria decide otro — que es
exactamente la forma del fallo que este producto ya pagó una vez, cuando el
encoder de zstd reservaba una ventana por core y se comía el presupuesto de 30
MB en reposo antes de que llegara un solo evento.

**Se cachean también los fallos.** Un evento que nombra un debug id que nadie
subió es el caso ordinario de cualquier front-end cuyo pipeline no sube mapas,
y sin recordar el fallo cada uno de sus eventos iría a la base de datos a que
le dijeran lo mismo. Un fallo recordado cuesta 512 bytes contra el presupuesto,
porque una caché de fallos sin precio es el mismo mapa sin tope con otro
nombre.

Un mapa más grande que el presupuesto entero no se cachea y no es un error: se
usa una vez y se suelta.

### Coste cero para quien no sube mapas

Antes de cualquier búsqueda, el caso de uso pregunta una vez —y recuerda diez
segundos, como el limitador con la configuración— si el proyecto tiene algún
artefacto. Una instalación que nunca subió un source map no paga una lectura de
base de datos por evento de JavaScript (ADR 005). Y un evento sin
`debug_meta` y sin `release` no pregunta nada: ninguna de las dos vías tiene
llave.

### Un artefacto ilegible nunca cuesta el evento

Un mapa que no se encuentra, no se lee o no se parsea cuesta la symbolication y
nada más. Una subida equivocada no puede parar el error tracking de un front-end
entero, y menos en silencio; el parseo fallido sí sale como `WARN`, porque el
único otro síntoma sería que la symbolication no hace nada.

### Almacenamiento

Tabla `artifacts` (migración **0024**) y `artifact_chunks` (**0025**), con el
esquema corregido según la grabación. Las dos mitades de source maps
escribieron esta tabla por separado —la lectura y la subida— y lo que quedó es
la de la subida, que es la que recibe los bytes y por tanto la que sabe qué hay
que poder reemplazar:

- `kind ∈ {minified_source, source_map}` — lo que dice el manifiesto, **no** el
  `{source, sourcemap}` que este ADR escribió de memoria. Un `CHECK` que
  discrepa de lo que va a llegar rechaza todas las subidas reales, y una
  migración se escribe una vez.
- `sourcemap_ref`, para la cabecera `sourcemap` que une script y mapa. Sin ella
  la vía 2 es adivinar.
- `bundle_debug_id`, la identidad propia del archivo, distinta del debug id de
  los ficheros que lleva dentro. Nada resuelve por ella; está para reconocer
  una re-subida.
- `release_id` NULL, `dist`, `name` normalizado a `~/ruta` por
  `domain.ArtifactURL`, `sha256`, `size` sin comprimir, `codec` y `blob`.
- `codec` junto al blob, como `events` con `payload_codec`: quien lee compara
  el codec de la fila con el suyo y se niega a descomprimir una que no puede
  honrar, en vez de entregar bytes zstd a lo que venga después.
- Dos índices UNIQUE **parciales** en vez de uno: `(project_id, debug_id,
  kind) WHERE debug_id <> ''` para lo moderno, y `(project_id,
  IFNULL(release_id,0), dist, name) WHERE debug_id = ''` para lo legado. Un
  único índice sobre todo no serviría: la cadena vacía no es identidad, y en
  SQLite dos NULL son distintos entre sí, así que una subida repetida se
  guardaría otra vez en cada deploy, para siempre.
- La normalización del nombre vive en el dominio y no en el repositorio,
  porque es donde se encuentran las dos mitades de la vía 2: el que sube manda
  `~/bundle.min.js` y el evento trae
  `https://app.example.com/static/bundle.min.js?v=8f3a`. Lo que el repositorio
  no puede saber es que un pipeline haya aplanado la ruta al subir, así que la
  lista de grafías candidatas se queda en symbolication, que es quien las
  deduce de un stack frame.
- `HasArtifacts`: una sola pregunta indexada, memoizada diez segundos, que
  sustituye a toda la ruta para un proyecto que nunca subió un mapa (ADR 005).

Presupuesto por proyecto y retención ligada a la release los implementa la
superficie de subida, que es quien recibe los bytes y quien puede cobrar el
presupuesto donde los hay (ADR 038).

### Lo que este ADR ya no dice

La hipótesis sobre el protocolo de subida —capacidades, chunks, assemble— queda
sustituida por lo grabado, y la subida se implementa desde ahí. Tres
cosas que no estaban aquí y deciden si funciona: el campo `accept` de
capacidades (anunciar sólo `artifact_bundles` hace que `sentry-cli` **no** use
debug ids), no anunciar `artifact_bundles_v2`, y que `ok` en `assemble` sólo se
puede contestar después de tener el bundle de verdad.

## Anexo — la superficie de subida, corregida contra la grabación

Lo que sigue se escribió al implementar la subida. No invalida nada de
lo de arriba; corrige cinco cosas que, tal como estaban escritas, habrían
fallado **en silencio**, y añade el contrato de respuesta, que era la mitad del
trabajo y no estaba.

### 1. Los nombres de `kind` son los del manifiesto, no los de este ADR

El manifiesto dentro del bundle dice `minified_source` y `source_map`. Arriba
ponía `source` y `sourcemap`. Un `CHECK` escrito desde el ADR habría rechazado
la primera subida real, y una migración se escribe una vez: la 0024 acepta lo
que va a llegar, según la grabación.

### 2. Faltaban dos columnas, y una de ellas hace falta para la vía 2

- `sourcemap_ref`: la cabecera `sourcemap` del script, que nombra su mapa **por
  nombre de fichero** y no por url. Es lo único que une script y mapa cuando no
  hay debug id.
- `bundle_debug_id`: el manifiesto tiene un `debug_id` **propio del bundle**,
  distinto del de los ficheros. Nada lo consulta; se guarda para que un
  operador pueda distinguir de qué subida vino un fichero.

El script y su mapa **comparten** el `debug_id`. Buscar por debug id devuelve
dos filas y hay que pedir también el `kind`, o la mitad de las veces el
resolver recibe el script minificado que estaba intentando resolver.

### 3. `accept` decide el protocolo, y no aparecía en ninguna parte

Medido contra seis variantes:

| `accept` | Resultado |
|---|---|
| `["release_files","artifact_bundles"]` | artifact bundle, con debug ids |
| `["artifact_bundles"]` | **cae a legacy**: *"a release is required"* |
| `["release_files"]` / `[]` / ausente | *"does not support artifact bundles"* |

**Anunciar sólo `artifact_bundles` —que es lo que se deduce de "debug IDs
primero"— es exactamente lo que apaga los debug ids.** `release_files` no es un
gesto de compatibilidad: es un requisito del camino moderno.

### 4. `artifact_bundles_v2` no se anuncia, y esta es la razón

Con él, la herramienta llama a `assemble` **primero**, antes de subir nada,
para que el servidor le diga qué chunks le faltan. Contra un servidor que
conteste `ok` alegremente lo medido fueron tres peticiones, **cero chunks
subidos** y `sentry-cli` declarando éxito: los source maps de un build entero
perdidos sin un error en ninguna parte (medido).

El flujo v1 hace el mismo trabajo con una forma menos de perder ficheros, y a
este servidor v2 no le compra nada: los chunks se guardan por hash de
contenido, así que la deduplicación que v2 negocia aquí ya es gratis.

### 5. El contrato de respuesta

- **Capacidades**: seis campos obligatorios (`url`, `chunkSize`,
  `chunksPerRequest`, `maxRequestSize`, `hashAlgorithm`, `concurrency`); falta
  uno y la herramienta aborta nombrándolo. `hashAlgorithm` es un **enum**:
  `"sha256"` es un error de parseo, no otro hash. La `url` es **absoluta** y se
  construye desde el origin configurado, porque el cliente postea los chunks a
  ese campo y no a la ruta que preguntó.
- **Chunks**: el nombre del campo multipart es `file` o `file_gzip` —lo decide
  lo que el servidor anunció en `compression`— y es el mismo para todos los
  parts de la petición. Lo que los distingue es el **`filename`**, que es el
  **sha1 del contenido descomprimido**. Un servidor que verificara el sha1 de
  lo que llega por el cable rechazaría a todo cliente que anuncie gzip —es
  decir, a todos los que él mismo mandó usarlo— con sus tests en verde
  (medido; misma familia que ADR 002).
- **Assemble**: objeto **plano** con `state ∈ {ok, created, assembling,
  error}`. Un mapa por checksum, que es lo que sugiere la forma de la petición,
  para la herramienta con `missing field \`state\``.
- **Ficheros de release**: el objeto vuelve con `id`, o la herramienta falla.
- **Deduplicación**: `GET …/releases/{v}/files/?checksum=…&cursor=` se emite
  **siempre que hay `--release`**. Da igual lo que se conteste —medido— pero
  está en el camino feliz: un 404 ahí sería contestar 404 a algo normal. Se
  contesta `[]`, como en la grabación.

### 6. La regla que no es de formas

`{"state":"ok"}` se cree a pies juntillas. Un servidor que lo conteste sin
tener el bundle le ha dicho a la herramienta que la subida fue bien, y nadie se
entera hasta que semanas después alguien abre un stack trace minificado.

**`ok` se produce en un solo sitio** (`usecase.Artifacts.AssembleBundle`):
después de encontrar los chunks, concatenarlos en el orden que dio el cliente,
verificar el checksum del conjunto, leer el ZIP y su manifiesto, y escribir.
Todo lo anterior es `created`, que es lo que hace que el cliente vuelva.
`scripts/sourcemaps.sh` comprueba que esa comprobación tiene dientes: apunta la
misma herramienta a un servidor que miente y exige que el gate encuentre nada.

### 7. Dónde se cobra el presupuesto

Ver ADR 038. En resumen: el endpoint de chunks es de organización y **no nombra
proyecto**, así que ahí sólo cabe un techo de instalación sobre el área de
staging; el presupuesto por proyecto se cobra en el ensamblado, que es donde el
proyecto se conoce, y se reporta dentro del cuerpo — un `413` ahí sale como
*"unknown error"*.

### 8. El lector de bundles es un paquete puro

`internal/artifactbundle` lee y escribe el ZIP, sólo con stdlib, dentro del gate
de fronteras y de `depguard`. Es la misma razón que tiene el parser de
envelopes (ADR 002): son bytes que elige quien tenga un token de subida, y
encima comprimidos. Está fuzzeado (`make fuzz-bundle`), y sus límites se
aplican sobre lo que el archivo **expande**, no sobre el tamaño que declara —
que es un número que escribe el emisor.

Escribirlo primero fue deliberado: es lo único de la subida que la lectura
necesita.

## Consecuencias

- **Grouping usa el frame symbolicado cuando existe.** Esto **no** cambia
  `GroupingVersion` y no re-agrupa nada: el fingerprint sólo se calcula al
  ingerir, y ningún issue guardado se vuelve a mirar (ADR 003).

  Pero significa algo que hay que decir en voz alta y no esconder: **al activar
  source maps, el mismo error minificado de antes y el de después pueden ser
  dos issues.** El de antes se agrupó por `bundle.min.js` en `t`; el de después
  se agrupa por `checkout.js` en `decode`. Desde fuera se ve como un issue
  nuevo que aparece el día que alguien subió los mapas, y como un issue viejo
  que deja de recibir eventos. Es un corte de una sola vez por proyecto, no una
  re-agrupación continua, y es el precio de que la identidad se calcule sobre
  lo que la gente lee. La alternativa —agrupar por el frame minificado y
  enseñar el resuelto— cuesta un issue nuevo **en cada build**, que es peor y
  además permanente.

  Quien resuelva un issue minificado antes de subir mapas verá el equivalente
  resuelto reaparecer como issue nuevo, no como regresión. Es correcto: no es
  el mismo issue.
- El nombre de función se toma del `name` del token **sólo cuando el mapping
  trae uno**, y la mayoría no traen. Un source map nombra el token en la
  posición, no la función que lo contiene, así que inventarlo desde un segmento
  vecino produce un frame que dice `encoding` donde la función se llama
  `decode`. Quedarse con el nombre minificado es una mentira más pequeña que
  una confiada y equivocada. Recuperar el nombre real exige escanear el fuente
  minificado hacia atrás buscando la función que lo envuelve: queda para
  suspect commits y está anotado como deuda, no como olvido.
- `in_app` no se recalcula tras resolver. Un frame que resuelve a
  `node_modules/…` sigue marcado como in-app si el SDK lo marcó. Se decidirá
  con la UI de suspect commits, que es donde se nota.
- El evento almacenado gana `debug_meta` y `dist`, y cada frame resuelto gana
  `raw`. El payload crece; a cambio, un mapa subido mañana se puede comprobar
  contra los ids de un evento guardado hoy.
- La caché es de proceso y muere con él. Un reinicio cuesta un parseo por mapa
  activo, que es del orden de milisegundos y ocurre una vez.
- `internal/sourcemap` es una frontera nueva vigilada por dos gates (el test de
  arquitectura y depguard). Cualquier import del repo desde ahí rompe la
  compilación de `make check`, que es lo que la mantiene fuzzeable.

### De la superficie de subida

- Al activar source maps, los errores minificados viejos y los nuevos pueden
  ser issues distintos **una vez**: el fingerprint sólo se calcula al ingerir y
  los issues previos no se reagrupan.
- Un artefacto subido contra una release muere con ella, por el `ON DELETE
  CASCADE` de la 0024 — la regla de retención escrita donde no se puede
  olvidar. Uno subido sólo por debug id no nombra release ninguna, así que no
  hay con qué morir: se barre a los 30 días.
- Los chunks viven en SQLite y no en memoria. Un ensamblado que cayera en otro
  proceso —o en el mismo tras un reinicio— no encontraría nada y pediría la
  subida entera otra vez.
- No se promete compatibilidad con `artifact_bundles_v2`. Si una versión futura
  de la herramienta dejara de ofrecer el flujo v1, esto se revisa con una
  grabación nueva, no de memoria.

## Medición (2026-08-29)

- **Fixture real**: el envelope grabado resuelve el frame que se
  verificó a mano — generado `(3, 489)` → `../src/checkout.js` línea 10,
  columna 9, con la `context_line` del `throw`. Los otros tres frames del
  evento, que vienen del script no inyectado, se quedan como estaban.
- **Fuzz**: `make fuzz-sourcemap` 60 s → 23,9 M ejecuciones, 359 entradas
  interesantes, ningún fallo.
- **Coste**: con un evento de **12 frames todos resolubles** —el peor caso, y
  peor que un stacktrace real, que mezcla frames de bundles inyectados con
  frames de scripts que nadie subió— la symbolication conserva el **85–88 %
  de la capa de dominio** (decodificar, symbolicar, limpiar, re-encodear,
  agrupar), que es **≈ 4–5 % de la ruta de ingesta completa**, contra el
  presupuesto del 10 % que se fijó como objetivo. Tres repeticiones seguidas caen dentro de
  tres puntos: es una cifra reproducible.

  Se mide en esa capa y no sobre el socket porque **sobre el socket no se
  quedaba quieta**: tres ejecuciones del mismo binario dieron 78 %, 97 % y
  117 %. El motivo está en los p99 de 9–11 ms, que son el WAL de SQLite y la
  caché de páginas, no la ingesta. Un cociente entre dos medidas compone el
  error de las dos, así que un denominador que se mueve un 25 % entre
  ejecuciones adyacentes hace que un presupuesto del 10 % no se pueda medir.
  La capa de dominio no tiene disco, es donde la symbolication corre de
  verdad, y es la **más estricta** de las dos: el mismo coste absoluto es una
  fracción mayor de ella.

  Dónde se va: dos tercios en las líneas de contexto y en el payload más grande
  que producen (medido apagando el contexto: 12,0 % → 7,0 %), y el resto en la
  búsqueda binaria por frame. La parte cara —leer el blob y parsear el mapa— la
  paga el primer evento y la caché la absorbe para el resto.

  Dos cosas que el gate encontró y que valen como medición: resolver la URL de
  cada fuente con `url.Parse` **por frame** costaba 2,7 puntos, y se arregló
  parseando la base una vez por fichero generado y recordando cada fuente ya
  resuelta; y reservar la capacidad de los slices de contexto por adelantado
  costaba otra fracción. Sin esas dos, el coste era del 9,3 % de la capa por
  encima de lo que es ahora.
