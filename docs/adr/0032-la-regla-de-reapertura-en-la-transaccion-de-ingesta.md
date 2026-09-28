# 032 — La regla de reapertura se aplica en la transacción de ingesta

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación
- **Supersede**: el punto 4 de la decisión del [ADR 012](0012-releases-orden-y-resolucion-en-proxima-release.md) y su consecuencia sobre el campo `events`

## Contexto

El ADR 012 decidió que las columnas de resolución de `issues` —`first_release`,
`resolved_at`, `resolved_in_release`, `resolve_next_release`, `regressions`,
`regressed_in_release`, `seen_in_resolved_release_count`— vivieran en `issues`
pero **no** las escribiera `RecordEvent`. La razón era de coste de cambio:
`RecordEvent` pertenecía entonces a otra unidad de trabajo en vuelo, y tocar la
escritura más caliente del producto desde dos sitios a la vez es cómo se
pierden eventos.

El resultado fue un ciclo de ingesta en tres transacciones por evento:

1. `RecordEvent` almacenaba el evento y aplicaba la **regla vieja** — cualquier
   evento reabre un issue resuelto.
2. `Ensure` daba de alta la release del evento.
3. `ApplyEventRelease` releía la fila y **corregía** el veredicto: si el evento
   venía de la release en la que se resolvió, o de una anterior, deshacía la
   reapertura y contaba la supresión.

Eso es correcto de punta a punta —los tests de storage y el gate de releases
lo recorrían entero— pero deja dos cosas que ahora sí se pueden cerrar:

- **Una ventana en la que la fila almacenada miente.** Entre (1) y (3) el issue
  dice `unresolved` sobre un evento que va a resolverse como esperado. La
  ventana son microsegundos y una lectura concurrente la puede ver. Un producto
  cuyo argumento entero es "el estado del issue es creíble" no puede permitirse
  un estado intermedio que contradice el veredicto que está a punto de emitir.
- **Un commit de más por evento.** La transacción implícita de `Ensure` y, en
  los caminos de issue nuevo o reabierto, la transacción entera de
  `ApplyEventRelease`.

Además, `RecordEvent` ya no pertenece a otra unidad de trabajo. La condición
que motivaba la separación desapareció.

## Decisión

**Un evento es una transacción.** `RecordEvent` hace, en una sola transacción
y en este orden:

1. **Da de alta la release que el evento nombra** (el mismo `UPSERT` que
   `Ensure`, compartido en una función). Va **primero** porque el rango que
   ordena identificadores que no son versiones —un sha de git, un número de
   build— es el `id` de la propia fila de `releases`. Un evento del build que
   está emitiendo ahora tiene que ser una release conocida *antes* de que se
   compare con aquella en la que el issue se declaró arreglado; si no, la
   comparación la trataría como "nunca vista" y por tanto como más nueva
   (ADR 012, regla 4), reabriendo el issue sin evidencia.
2. **Lee el issue con sus columnas de resolución.**
3. **Si el issue está resuelto y anclado a una release**, lee el rango de las
   **dos** versiones que se comparan —no el orden entero del proyecto— y se lo
   pasa a `Issue.Observe` en `Observation.ReleaseOrder`. Es el único caso cuyo
   veredicto depende del orden; el evento común no paga nada por esto.
4. **`Issue.Observe` decide una vez.** Ya no hay una regla que se aplica y otra
   que la corrige: el dominio emite el veredicto correcto y lo que se escribe
   es ese.
5. **Escribe.** Si el evento movió el ciclo de vida del issue, las columnas de
   resolución van en la **misma** sentencia que los contadores. Si no —el caso
   masivamente mayoritario, otra ocurrencia de algo ya abierto—, la sentencia
   estrecha las omite: SQLite decide qué índices mantiene a partir de las
   columnas del `SET`, así que nombrar `first_release` y `regressed_in_release`
   costaría reescribir los dos índices de release en cada evento ingerido para
   guardar los valores que ya tenían.

`ApplyEventRelease` y `ports.ReleaseEvent` desaparecen. `Releases.OnEvent` y el
`Ingest.WithReleases` que lo cableaba también: el caso de uso de ingesta hace
**una** llamada, como antes de que existieran las releases.
`ReleaseRepository` conserva la mitad deliberada del ciclo de vida —una release
creada por una herramienta de despliegue, sus commits, sus deploys, y una
persona resolviendo, ignorando o reabriendo a mano— y `Ensure` sigue existiendo
como la forma sin transacción propia del mismo `UPSERT`.

**`events` en el detalle de una release deja de ser opcional.** El ADR 012
razonaba que el número vive en los agregados horarios (ADR 010) y que, mientras
esa tabla no existiera, la respuesta honesta era la ausencia del campo; el
repositorio lo comprobaba en `sqlite_master` en cada lectura. Los agregados son
las migraciones 0006/0007 y forman parte del esquema de **toda** instalación,
así que el condicional ya no puede ser falso: una rama que no se puede ejecutar
no se puede probar, y una que no se prueba deja de ser código y pasa a ser una
afirmación. El campo es un `int64` normal, siempre presente. Una release de la
que nadie ha enviado un evento reporta cero, que es la verdad, y no una
ausencia que el llamante tendría que distinguir de "este build no sabe".

## Consecuencias

- **La fila almacenada nunca contradice el veredicto.** No hay estado
  intermedio que un lector concurrente pueda ver, porque no hay dos escrituras.
- **`RecordEvent` escribe en `releases`.** El adaptador de issues toca una
  tabla que no es la suya, dentro del mismo paquete. Es exactamente lo que el
  ADR 010 ya hace con los tres buckets horarios, y por la misma razón: lo que
  tiene que ser consistente con un evento se escribe con el evento.
- **El fallo de la ingesta ya no es parcial.** Antes, un error en `Ensure` o en
  `ApplyEventRelease` se registraba y se tragaba, dejando un evento almacenado
  con un lado de releases a medias. Ahora o entra todo o no entra nada. La
  excepción deliberada: una versión que el producto **nunca podría volver a
  direccionar** —con una barra, o más larga que la columna— no da de alta la
  release y **no** hace fallar el evento. Se registra un aviso. Perder un
  reporte de error para proteger un listado es el intercambio al revés.
- **El evento que crea un issue escribe `first_release` en el `INSERT`.** Una
  segunda sentencia sólo podría decir lo mismo más tarde.
- **El coste medido** (i7-14650HX bajo WSL2, banco directo sobre el
  repositorio, 3000 eventos × 6 repeticiones, mediana):

  | camino | tres transacciones | una transacción | |
  |---|---|---|---|
  | issue nuevo | 731,0 µs/evento | **605,3 µs/evento** | **−17,2 %** |
  | issue existente | 365,8 µs/evento | **353,1 µs/evento** | **−3,5 %** |

  La diferencia entre los dos caminos es la esperada: en el issue existente lo
  que se ahorra es la transacción implícita del `UPSERT` de releases;
  `ApplyEventRelease` ya salía sin abrir ninguna. En el issue nuevo se ahorran
  las dos. El banco de extremo a extremo (`make bench`) no puede resolver una
  diferencia de este tamaño en esta máquina: su dispersión entre repeticiones
  es del 7 % al 44 %, más grande que el efecto. Se mide donde se puede medir.
- **Lo que el ADR 012 llamó "la salida identificada" queda tomada**, y con ella
  se recupera parte del 10–14 % que aquel trabajo costó. No todo: la parte que
  quedaba era un commit por evento, no los 150-220 µs que aquella medición le
  atribuyó — un número que, a la luz de la dispersión medida aquí, estaba
  contaminado por el ruido de la máquina más que por el commit.
