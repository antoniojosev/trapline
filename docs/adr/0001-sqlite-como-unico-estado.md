# 001 — SQLite como único estado (DuckDB diferido)

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

El producto entero se define por una restricción: **un binario, cero dependencias
externas**. El competidor al que se responde exige 54 contenedores y 16 GB de RAM.
Cada dependencia de infraestructura que se añada erosiona la única razón por la que
alguien elegiría esto.

Un almacén columnar (DuckDB, ClickHouse) es tentador: los datos son series
temporales y las consultas son agregaciones. Es la elección "correcta" si se mira
solo el perfil de queries.

## Decisión

**SQLite en modo WAL como único estado**: proyectos, issues, eventos, agregados y
configuración. **DuckDB queda diferido**, no descartado.

Lo que hace viable a SQLite aquí no es SQLite: son los **agregados
pre-calculados**. Ninguna consulta de listado, búsqueda o dashboard escanea payloads
de eventos — pegan contra columnas indexadas y tablas de agregados escritas en
ingesta.

## Consecuencias

- Backup y restore son operaciones de un fichero — pero **no con `cp`**: copiar una
  base WAL en caliente puede producir una copia corrupta (db y `-wal`
  inconsistentes). Se expone un comando `backup` que usa la API de backup online.
- `auto_vacuum=INCREMENTAL` se activa **al crear el esquema**. Activarlo después
  exige un `VACUUM` completo, exactamente lo que la política de retención prohíbe
  hacer en caliente.
- Un único escritor. La ingesta escribe en batches (transacciones agrupadas cada N
  eventos o T ms), que es además como SQLite rinde mejor.
- El volumen objetivo declarado es <100 eventos/s sostenidos. **Esto es una
  promesa verificable**: hay un benchmark de ingesta como gate de CI, no una
  afirmación de README.

## Medición real (2026-08-24)

El benchmark ya existe (`make bench`) y el objetivo se cumple **por 6-7×**:
548-747 eventos/s sostenidos end-to-end sobre HTTP, con un evento realista de
14,6 KB (excepción encadenada, 12 frames con contexto, 20 breadcrumbs, tags,
request, user, contexts, extra), en un i7-14650HX bajo WSL2 y con carga.

Dónde se va el tiempo, por evento: **trabajo de dominio 35-45%** (decodificar
JSON 238 µs, scrubbing 96 µs, re-encodear 69 µs), **transacción SQLite 35-50%**
(360 µs, de los cuales 121 µs son los upserts de tags), **HTTP + auth + limiter
~14%**, descompresión y framing ~3%.

Hallazgo que ordena las prioridades: **decodificar el JSON del evento cuesta más
que toda la transacción de SQLite**. Cualquier optimización futura empieza ahí.

**Dos correcciones a esta ADR, hechas por la medición:**

1. **El batching que este documento describía nunca se implementó**, y la
   redacción se ha eliminado en vez de implementarlo. `RecordEvent` hace una
   transacción por evento. El benchmark midió lo que valdría agrupar: **~1,5×**
   (84 µs/evento a una transacción por evento frente a 54 µs/evento a 50), no el
   salto que sugiere "SQLite ama los batches", porque aquí domina el coste por
   sentencia y no el fsync. A 6-7× por encima del objetivo, y con el coste de
   introducir una ventana en la que un evento se ha confirmado al SDK pero
   todavía no es durable —justo lo que un tracker de errores no debe permitirse—
   no se hace. La puerta sigue abierta: `engine.Store.Append` recibe un slice
   precisamente para esto, cuando el volumen lo justifique.

2. **La concurrencia no ayuda**, y es la conducta correcta: 8 clientes
   simultáneos dan 667-1090 ev/s frente a 705-747 con uno solo. El escritor
   único convierte la concurrencia en cola, que es exactamente lo que este
   documento dice que hace.

Nota sobre la variabilidad, porque condiciona el gate: la ruta que **crea** un
issue (INSERT, con el índice único creciendo) tiene 59-61% de dispersión entre
repeticiones y picos de p99 a 20 ms, mientras que la ruta que **acumula** en un
issue existente (UPDATE) es plana, con 3-19%. Un benchmark que solo enviara un
error repetido no habría visto ni la forma ni la inestabilidad.
- Si algún día el volumen real desborda esto, la salida es el puerto de storage
  (ADR 004): se añade un adapter columnar sin tocar dominio. Introducir un segundo
  motor de storage *ahora*, sin necesidad demostrada, sería pagar complejidad por
  adelantado contra un problema hipotético.

## Medición (2026-08-29): lo que cuestan los agregados

El ADR 010 añadió a la transacción de ingesta hasta cuatro `UPSERT` por evento —los
buckets por hora que hacen que este documento pueda seguir diciendo que ninguna
consulta escanea payloads—. El coste medido con el mismo benchmark y la misma
máquina:

- **new issue**: 694,5 → 607,0 ev/s
- **existing issue**: 746,6 → 718,7 ev/s
- El escalón «transacción SQLite» pasa de 722,8 µs a 790,0 µs por evento.

Sigue en 4,0× por encima del gate de 150 ev/s y 6× por encima del objetivo
declarado de 100 ev/s sostenidos. Merece anotarse aquí porque es la primera vez que
una decisión de diseño gasta throughput a propósito, y el reparto del coste no ha
cambiado: **decodificar el JSON del evento sigue costando más que toda la
transacción de SQLite** (421,9 µs contra 790,0 µs para cuatro sentencias más de las
que había), así que la prioridad de optimización que este documento fijó no se
mueve.

## Corrección (2026-09-20): la cifra del incumbente

El Contexto de arriba dice que el competidor «exige 54 contenedores y 16 GB de
RAM». **El 54 nunca se verificó.** Se escribió de memoria en agosto, cuando
este documento se redactó, y nadie fue a contarlos.

En septiembre sí se fue a mirar, al escribir
[`docs/benchmarks/footprint.md`](../benchmarks/footprint.md): su `docker
compose` de `self-hosted` despliega **~40 contenedores**, y 4 núcleos / 16 GB /
20 GB es su **mínimo declarado**, no un consumo medido por nosotros. Esa es la
cifra que sigue el README y la que va en cualquier material público.

El texto original no se edita, que es la regla de este directorio: registra lo
que se creía el 2026-08-22 y con qué se decidió. Esta sección se añade debajo
por la misma vía que las dos mediciones anteriores.

**La decisión no depende del número.** El argumento es «decenas de contenedores
y gigabytes de RAM contra un binario y megabytes», y se sostiene igual con 40
que con 54: a 34,2 MB con todo encendido, el orden de magnitud no lo mueve un
factor de 1,35. Lo que cambia es dónde puede decirse cada cifra — aquí, en un
documento histórico que explica por qué se eligió SQLite, un número aproximado
de agosto no hace daño; en un post de lanzamiento, sí.
