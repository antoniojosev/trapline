# La huella: qué cuesta de verdad tener esto encendido

Este producto se vende por una sola cifra —cabe en una máquina pequeña— así que
la cifra tiene que ser medible, reproducible y capaz de romper el build cuando
deje de ser cierta. Eso último es lo que hace `scripts/footprint.sh`, que es la
mitad ejecutable de este documento: lo que aquí se publica, allí se comprueba.

> **Cómo reproducirlo**
>
> ```sh
> make footprint            # mide, imprime la tabla, y falla si alguna cifra se rompió
> ./scripts/footprint.sh --json
> ./scripts/footprint.sh --quick   # una décima parte del volumen, para desarrollar
> ```
>
> No hace falta Docker, ni red, ni nada instalado aparte de Go. El gate corre
> cada noche en `nightly.yml`.

---

## 1. Qué se mide, y por qué tres números y no uno

Cada perfil se mide **de tres maneras**:

| número | qué es | por qué está |
|---|---|---|
| **en reposo** | `VmRSS` después de dejar de usarlo y esperar al recolector | la cifra que alguien compara con la RAM de su VPS |
| **pico ingiriendo** | `VmHWM` durante la carga | lo que hace falta tener libre cuando llega el tráfico |
| **pico siendo leído** | `VmHWM` durante una ráfaga de lecturas concurrentes | el dashboard, la búsqueda y el bundle tienen su propia historia de memoria, y no es la de la ingesta |

Una huella citada de un proceso en reposo es la cifra que es cierta cuando
nadie está usando el producto, que es justo el único momento que no le importa
a nadie. Este repositorio ha publicado **tres** cifras de memoria equivocadas y
las tres lo eran de esa forma: Argon2id presupuestado por hash, el encoder de
zstd medido en una máquina de dos núcleos, y `/login` sin techo de intentos
concurrentes, que convirtió una huella de 30 MB en 1,03 GB con 200 logins a la
vez (ADR 023). La cuarta la encontró este mismo documento y está en §4.

El pico se lee de **`VmHWM`**, la marca de agua que mantiene el kernel, y no de
un muestreo de `VmRSS`. Un pico ya recolectado es invisible para una muestra
posterior, y un pico es exactamente lo que se está buscando. Antes de cada
medición se reinicia la marca escribiendo `5` en `/proc/<pid>/clear_refs`, así
que lo que se lee después pertenece a lo que pasó después y no al Argon2id del
`setup`.

## 2. Los dos perfiles

**Mínimo.** Instalación recién creada. Un proyecto aceptando errores, que es el
perfil por defecto, y **un** job de fondo: `retention`, el único subsistema en
el que nadie opta por entrar (ADR 005, ADR 014). El gate comprueba que sea
exactamente uno: cero significaría que el disco crece para siempre, y más de
uno significaría que arrancó algo que nadie encendió.

**Todo encendido.** Absolutamente todo lo que este producto sabe hacer, a la
vez:

- errores, con sus agregados horarios y el índice FTS5 (ADR 010, ADR 011);
- **source maps**: un bundle real subido por la ruta real, con su blob zstd en
  la tabla de artefactos y la caché de symbolication detrás (ADR 018);
- **tracing**: transactions con `traces_sample_rate` al 100 %, los sketches de
  latencia por minuto y el job `downsample` (ADR 020, ADR 021);
- **release health**: sessions reales, la ventana acotada en memoria y sus
  contadores horarios (ADR 008);
- alertas: un canal con su secreto cifrado en disco, una regla, y el job
  `notifier` que existe porque el canal existe (ADR 015);
- **diez monitores**: cinco cron y cinco uptime, con los dos jobs que los
  vigilan (ADR 016, ADR 037);
- la página de estado pública y el digest semanal (ADR 017, ADR 035).

Un perfil que dejara fuera los subsistemas añadidos al final publicaría la
huella de un producto que ya no existe. Por eso el gate también comprueba que
haya **al menos seis** jobs de fondo corriendo en este perfil y los nombra: si
un subsistema no arrancó, la cifra saldría falsamente buena, que es la
dirección en la que miente un benchmark cuando nadie lo está mirando.

**Lo que este perfil no incluye:** el **servidor MCP** (ADR 022). `trapline
mcp` es un proceso aparte, por stdio, que actúa como cliente REST de este
servidor; `POST /mcp` sí vive dentro del binario, pero no tiene estado propio
ni job de fondo —es otra proyección de los mismos casos de uso—, así que no
añade nada en reposo. Lo que sí cuesta el MCP es **tamaño de binario**, y ese
coste está en la tabla: el SDK oficial son ~1,7 MB de los 18,13 MB.

### Volumen

Los valores por defecto, que son los que se publican:

| | |
|---|---|
| eventos | 100 000 |
| issues distintos | 1 000 |
| transactions | 20 000 |
| sessions | 10 000 |
| monitores | 10 |

**Mil issues distintos y no cien mil copias de un error.** El índice, la tabla
de búsqueda y los buckets horarios escalan con el número de issues *distintos*,
no con el de eventos: cien mil veces el mismo error es una fila en casi todos
los sitios que cuestan algo. Un generador que repitiera un evento publicaría
una huella que ninguna instalación real tiene.

El tope por proyecto se sube durante la carga y sólo durante la carga. Es
protección contra picos —12 000 eventos por minuto y categoría por defecto
(ADR 005)—, así que con el valor de serie llenar la instalación serían ocho
minutos de espera deliberada y el gate estaría midiendo el reloj del limitador.
No cambia la cifra: un techo es un contador, y lo que se mide es lo que cuestan
cien mil eventos una vez están dentro. La primera versión de este script no lo
subía y **1 913 de 2 600 envelopes volvieron con 429**; la comprobación que lo
cazó sigue en el gate, porque una instalación sembrada con un tercio de lo que
dice tener publicaría la huella de un producto que nadie ha instalado.

## 3. Las cifras

Medidas en la máquina de desarrollo (WSL2, Linux 6.6, Go 1.26.6, SQLite puro Go
sin CGO). **Un número de una sola máquina es una anécdota**: lo que hace que
signifiquen algo es que el gate las vuelve a medir cada noche y falla si se
rompen, no que se midieran una vez.

| perfil | en reposo | pico ingiriendo | pico leyendo | en disco | jobs |
|---|---|---|---|---|---|
| **mínimo** | **19,7 MB** | n/a | n/a | 4 KB | 1 |
| **todo encendido** | **34,2 MB** | **38,7 MB** | **38,0 MB** | 86,1 MB | 6 |

- **Binario: 18,13 MB**, contra un presupuesto de 30 MB que hace fallar el
  build (`make size`). De esos, ~1,7 MB son el SDK oficial de MCP; el resto de
  subsistemas no añaden dependencias.
- **Disco: 695 bytes por item**, sobre 130 000 items entre eventos,
  transactions y sessions. Incluye el payload comprimido con zstd, los índices,
  el índice FTS5 de issues, los buckets horarios, los sketches de latencia por
  minuto, el bundle de source maps y los resultados de uptime.
- **Sembrado a 1 166 eventos/s** con ocho clientes concurrentes. Eso es cuánto
  tardó en llenarse, no una promesa de rendimiento — §6.
- Los seis jobs de fondo del perfil cargado: `retention`, `notifier`,
  `cron-watch`, `uptime`, `downsample` y `session-flush`. En el perfil mínimo
  hay exactamente uno, `retention`.

La diferencia entre los dos perfiles —unos **14 MB**— es lo que cuestan, juntos,
la caché de source maps, la ventana de sessions en memoria, los sketches en
vuelo, las conexiones de SQLite que abren los cinco jobs extra y cien mil
eventos de índices calientes. No es lo que cuesta *tener encendida* cada
funcionalidad: un subsistema apagado no arranca su goroutine y no aparece
(ADR 005, ADR 014), y el perfil mínimo es la prueba.


## 4. Bajo ataque: lo que cuesta una avalancha, no lo que cuesta una petición

Un endpoint de ingesta público tiene una cuarta cifra, y es la que más importa
de todas: **cuánta memoria puede hacerle gastar alguien que no es un cliente**.

Los topes por petición estaban puestos y eran correctos —20 MiB por envelope,
4 MiB por item, 100 items, y un lector acotado que corta la expansión de gzip y
zstd—. Ninguno responde a la pregunta del atacante, que no es «¿cuánto cuesta
una?» sino «¿cuánto cuestan mil?». Nada limitaba cuántas peticiones podían
sostener esos topes a la vez.

Medido con `scripts/hardening.sh`, que manda la misma bomba a dos
concurrencias justamente para poder comparar:

| ataque | subida del atacante | antes | después |
|---|---|---|---|
| 128 bombas gzip a 976:1, concurrencia 32 | 3,2 MB | 649 MB | 121 MB |
| 512 bombas gzip a 976:1, concurrencia 128 | 13,2 MB | *crecía* | 127 MB |
| 128 bombas zstd a 21 346:1, concurrencia 32 | 151 KB | 1,46 GB | 93 MB |
| 512 bombas zstd a 21 346:1, concurrencia 128 | 604 KB | *crecía* | 121 MB |

Lo importante de esa tabla no es que las cifras sean más pequeñas. Es que son
**planas**: cuadruplicar la avalancha las mueve un 4 % y un 30 %, no un 400 %. Antes
no había ninguna cifra que publicar aquí, porque la cifra la elegía quien
atacase. El arreglo es un presupuesto de memoria de 32 MiB compartido por todas
las peticiones de ingesta en vuelo, reservado por adelantado para el working
set del descompresor y cobrado a medida que se producen bytes (ADR 039).

Y el gate lo afirma como propiedad, no como número: *cuadruplicar el ataque no
puede cuadruplicar el coste*. Es una afirmación que una sola petición no puede
hacer ni romper, que es la lección de las tres huellas equivocadas escrita en
forma de assert.

## 5. La comparativa, con sus asteriscos

La comparación honesta no es «nuestra medición contra su medición»: no hemos
levantado la pila del incumbente para medirla, y decir lo contrario sería
inventar. Lo que sí se puede comparar es **lo que cada proyecto te dice que
provisiones**, que es la decisión que alguien toma antes de instalar nada.

| | esto | `sentry` self-hosted |
|---|---|---|
| lo que hay que provisionar | 1 núcleo, 512 MB de RAM, disco para los datos | **4 núcleos, 16 GB de RAM, 20 GB de disco** (mínimo declarado en su repo `self-hosted`) |
| qué se despliega | un binario estático | ~40 contenedores por `docker compose` |
| qué más hace falta | nada | Kafka, ZooKeeper, ClickHouse, PostgreSQL, Redis, Memcached, Snuba, Relay, Symbolicator |
| cómo se actualiza | reemplazar el binario | `install.sh`, con migraciones de varios servicios |
| copia de seguridad | copiar un fichero | pg_dump + ClickHouse + los volúmenes |

Los asteriscos, que importan tanto como la tabla:

- **16 GB es su mínimo declarado, no su consumo medido.** Un mínimo publicado
  es conservador por diseño y cubre picos que aquí no se están provocando.
- **No hacen lo mismo.** Esa pila procesa el volumen de miles de
  organizaciones, guarda cada session por separado para hacer analítica de
  sesiones, y tiene un motor de búsqueda de eventos entero (Snuba sobre
  ClickHouse) que aquí no existe y no va a existir: los listados, la búsqueda y
  el dashboard de este producto pegan sólo contra columnas indexadas y tablas
  agregadas, y jamás escanean payloads (ADR 001). Esa decisión es lo que hace
  posible la columna de la izquierda, y también es lo que este producto no
  sabrá hacer nunca.
- **La comparación es de instalación pequeña.** A escala de una empresa grande,
  la columna de la derecha es la que tiene respuestas y ésta no.
- **Verifica la cifra antes de citarla.** Los requisitos mínimos publicados de
  `self-hosted` han cambiado más de una vez (fueron 8 GB antes de 16 GB). Este
  documento la cita tal y como estaba en septiembre de 2026; si vas a ponerla en
  un README o en un hilo, mírala otra vez.

## 6. Qué **no** dice este documento

- **No dice qué throughput sostiene esto.** Eso es `make bench`, que es un gate
  aparte con su propio umbral y su propia razón (ADR 001). La cifra de eventos
  por segundo que imprime este script es cuánto tardó en sembrarse la
  instalación, no una promesa de rendimiento.
- **No dice cuánto disco vas a gastar tú.** El coste por evento depende del
  tamaño de tus payloads, y un stacktrace de Java con doscientos frames no se
  parece a un `ValueError` de Python. La cifra de bytes por item es la de los
  eventos que genera `compat/load`, que son de tamaño realista pero son los
  nuestros.
- **No dice nada de la huella en disco a largo plazo.** La retención por
  categoría (eventos 90 días, agregados 400, resultados de uptime 90) es lo que
  la gobierna, y una instalación que lleve un año encendida es un experimento
  que este gate no hace.
