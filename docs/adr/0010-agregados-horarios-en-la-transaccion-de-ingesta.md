# 010 — Agregados horarios escritos en la transacción de ingesta

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El panel tiene que responder cuatro preguntas, y las cuatro son agregaciones sobre
rangos de tiempo: cuántos errores por hora, qué issue es el más ruidoso *ahora*,
qué release lo trajo y si esto pasa sólo en staging.

La implementación obvia es un `GROUP BY` sobre `events` al leer. Tiene dos
problemas, y el segundo es el que decide:

1. **Viola la regla de oro del ADR 001.** Ninguna consulta de listado, búsqueda o
   dashboard escanea payloads. Un `GROUP BY` por rango sobre `events` es
   exactamente ese escaneo, y crece con el volumen que el producto promete
   aguantar.
2. **Deja de funcionar el día que la retención borra eventos** — que es
   precisamente el día en que el dashboard hace más falta. Los payloads caducan a
   los 90 días por defecto; la pregunta "¿esto está peor que el trimestre pasado?"
   no caduca. Un dashboard que pierde la historia cuando caducan los payloads no
   es un dashboard, es una vista de los últimos noventa días con otro nombre.

## Decisión

**Tres tablas de buckets por hora, actualizadas con `UPSERT` dentro de la misma
transacción que `RecordEvent`:**

- `issue_hourly(issue_id, project_id, hour, count)`
- `project_hourly(project_id, hour, level, count)`
- `project_hourly_dims(project_id, hour, dim ∈ {release, environment}, value, count)`

La hora es UTC, `TEXT`, formato `YYYY-MM-DDTHH`. Sin buckets por minuto para
errores: los de tracing sí los tendrán, con sketch (ADR 007).

**La hora sale de cuándo ocurrió el evento, no de cuándo llegó.** Un móvil que
estuvo una hora sin red tiene que caer en la hora en que se rompió; si no, un corte
de red se dibuja como un pico en el momento en que volvió la conexión.

**Retención propia**: categoría lógica `aggregates`, **400 días** por defecto,
independiente de los 90 de `events`. `aggregates` **no** es una categoría de
ingesta: nadie puede enviarla ni habilitarla, porque se escribe como efecto
lateral de guardar un evento. Por eso `engine.Categories()` y
`engine.RetentionCategories()` son listas distintas — la primera responde "¿qué
puede aceptar un proyecto?", la segunda "¿qué borra una barrida?".

## Consecuencias

- **En la misma transacción, no en una cola.** Un contador que puede discrepar de
  los eventos que cuenta es un dashboard en el que nadie confía dos veces. La cola
  compraría microsegundos a cambio de una ventana en la que las cifras mienten.
- **El coste va en el camino crítico de ingesta**, así que se mide, no se supone.
  Ver la sección de medición.
- Un evento sin release y sin environment no escribe fila de dimensión alguna, en
  vez de escribir una bajo `""`. La ausencia ya es la respuesta: esos eventos no lo
  dijeron. Un breakdown cuyo mayor bucket es la cadena vacía no le dice nada a
  nadie.
- La barrida de agregados **redondea el corte a su bucket**, así que una hora
  parcialmente caducada sobrevive entera. Borrarla eliminaría también los eventos
  de la mitad nueva de esa hora, y un gráfico que pierde su barra más reciente en
  cada barrida es peor que uno que conserva una hora de más.
- `issue_hourly` cae en cascada con su issue. Buckets huérfanos seguirían sumando
  en la serie del proyecto sin nada a donde navegar.
- El gate `scripts/stats.sh` verifica la propiedad entera, incluido el caso que
  justifica la decisión: con `events` a 0 días y una barrida hecha, la serie, el
  top, el breakdown y el gráfico del issue siguen respondiendo lo mismo.

## Medición (2026-08-29)

Los upserts añaden **hasta cuatro sentencias por evento** (issue, nivel, release,
environment). Medido con `make bench` en el mismo i7-14650HX bajo WSL2 que midió el
ADR 001, mejor de cinco repeticiones, con el mismo evento realista de 14,6 KB:

| Workload | Antes | Después | Coste |
|---|---|---|---|
| new issue | 694,5 ev/s | 607,0 ev/s | −12,6 % |
| existing issue | 746,6 ev/s | 718,7 ev/s | −3,7 % |

En microsegundos por evento, el escalón de la transacción SQLite pasa de 722,8 µs a
790,0 µs: **+67,2 µs**, unos 17 µs por upsert. Está por debajo de los 30–40 µs que
se anticiparon, y el resto del camino —descomprimir, parsear el envelope,
decodificar, scrubbing, HTTP— no se mueve, que es la comprobación de que el coste
está donde se esperaba y no en otro sitio.

Los dos workloads pagan distinto y la diferencia es la esperable: crear un issue ya
era la ruta cara y con más dispersión (32,9 % entre repeticiones, contra 22,4 % la
otra), porque el índice único de `issues` crece mientras se mide; añadirle cuatro
inserciones en tablas que también están creciendo se nota más que añadírselas a una
ruta que sólo incrementa contadores existentes.

El gate está en 150 ev/s y el peor de los dos workloads queda **4,0×** por encima.
La decisión se toma con ese margen a la vista: si el coste hubiera hundido el
throughput por debajo del gate, el problema habría sido el diseño del índice, no el
umbral.
