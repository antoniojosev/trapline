# 011 — Búsqueda de texto con FTS5 sobre issues, nunca sobre eventos

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El listado ya aceptaba un `q`, implementado como `lower(title) LIKE '%x%' OR
lower(culprit) LIKE '%x%'`. Un `LIKE` con comodín inicial no puede usar ningún
índice: es un escaneo de todos los issues del proyecto en cada pulsación de tecla.
A 10⁵ issues, la caja de búsqueda pasa a ser lo más lento del producto — y lo es
justo cuando más issues hay, que es cuando buscar sirve para algo.

La alternativa tentadora es indexar los eventos: ahí está el texto de verdad
—stacktraces, breadcrumbs, contexto—. Es exactamente lo que el ADR 001 prohíbe: un
índice del tamaño del almacén, reconstruido en cada evento, sobre payloads que la
retención va a borrar de todos modos.

## Decisión

**Tabla virtual `issues_fts` (FTS5, `content='issues'`, tokenizer
`trigram remove_diacritics 1`) sobre tres columnas: `title`, `culprit` y una
columna nueva `issues.sample_message`.**

**El tokenizer es `trigram`, y esa es la decisión, no un detalle de ella.**
`unicode61` parte por límites de palabra, así que sólo puede emparejar una
palabra entera o su prefijo: `Value` encuentra `ValueError` y `Error` no. En un
producto cuya materia prima son nombres de excepción eso no es un borde, es el
uso normal — `ConnectionTimeoutError`, `ECONNREFUSED`, `SQLSTATE[23000]` son un
único token cada uno, y quien busca `Timeout`, `REFUSED` o `SQLSTATE` está
buscando el medio de uno. `trigram` indexa cada ventana de tres caracteres, de
modo que un `MATCH` es una búsqueda de subcadena servida por el índice: la misma
semántica que tenía el `LIKE '%x%'` que se sustituye, sin el escaneo.

`sample_message` es el mensaje del **último** evento, escrito en `RecordEvent`
dentro de la misma transacción que los contadores. Existe porque un issue titulado
sólo con un tipo de excepción no es encontrable por nada que el usuario recuerde;
el mensaje sí lo es. No vive en `domain.Issue`: ninguna regla de dominio lo lee, y
ponerlo ahí sería que el almacenamiento use el dominio para cargar equipaje propio.

**Los términos del usuario se escapan como frases** (`"término"`) y se unen con la
conjunción implícita de FTS5. El lenguaje de consulta de FTS5 **no se expone**:
quien busca `NOT NULL` quiere esas dos palabras, no un operador booleano, y quien
busca `*` quiere un asterisco. No hay `*` final: con este tokenizer un prefijo es
sólo una subcadena que empieza al principio, así que el comodín sería sintaxis sin
nada que hacer.

**Términos de menos de tres caracteres**: el índice está construido con ventanas de
tres, así que un término de uno o dos no tiene ventana que consultar. Se
**descartan** cuando hay al menos otro término utilizable —"de" y "la" son como
escribe la gente, y dos letras no acotan nada de todos modos— y si no queda
ninguno, la búsqueda **se rechaza** con `ErrInvalidSearch`, un 400 y un mensaje
que dice cuál es el mínimo. Lo que no puede pasar es devolver "sin resultados" a
una consulta que nunca se ejecutó: una página vacía es indistinguible de "no hay
coincidencias" y manda a alguien a buscar un bug en sus propios datos.

**Verificación en el arranque.** `Open` construye una tabla virtual FTS5 de prueba
—**nombrando el tokenizer `trigram`**, porque el módulo y el tokenizer son
separables— en el esquema `temp` antes de migrar, y falla con un mensaje accionable
si falta cualquiera de los dos. `modernc.org/sqlite` v1.57 trae ambos, sobre SQLite
3.53.3: verificado ejecutándolo, no leyendo la documentación. Un driver que un día
dejara de traerlos convertiría cada búsqueda en una consulta que devuelve cero
resultados para siempre, y una caja de búsqueda sin resultados es indistinguible de
una búsqueda sin coincidencias. Se comprueba construyendo la tabla y no leyendo una
opción de compilación, porque la opción es lo que el driver declara y la tabla es lo
que el driver puede hacer.

## Consecuencias

- **El índice ocupa unas 5,5 veces más**, y es el precio de la decisión. Sobre un
  corpus de 20 000 issues realistas (título de excepción, culprit con ruta y
  mensaje en español): 2,87 MB con `unicode61` frente a **15,77 MB** con `trigram`,
  es decir 144 → **789 bytes por issue**. Es una tabla de issues, no de eventos: a
  100 000 issues son ~79 MB contra un almacén que a ese volumen se mide en
  gigabytes de payloads. El índice es un redondeo del fichero, no un sumando.
- **Sin acentos, en ambas direcciones.** `remove_diacritics 1` pliega índice y
  consulta, así que "función" y "funcion" encuentran lo mismo y ninguna grafía está
  privilegiada. Es la diferencia entre que la búsqueda sirva o no en español, y
  está verificado con el tokenizer nuevo y no heredado del anterior: la suite de
  compatibilidad manda mensajes con acentos y habría sido una regresión silenciosa.
- **Una búsqueda de una o dos letras deja de funcionar** y pasa a ser un error
  explícito. Es la única pérdida frente al `LIKE`, y es un cambio de
  comportamiento visible: `issues list -q ab` sale con código 1 en vez de con una
  lista vacía.
- **El índice sigue al mensaje más reciente**, no acumula todos los que un issue
  tuvo. Un índice acumulativo devolvería issues por texto que ya no se puede ver en
  ninguna parte.
- **El trigger de actualización está acotado dos veces** (`AFTER UPDATE OF …` más
  una cláusula `WHEN` que compara valores), y las dos acotaciones están en el
  camino crítico de ingesta: cada evento actualiza `last_seen` y el contador de su
  issue, y sin ellas eso reindexaría el issue una vez por evento para registrar que
  cambió un número. El caso común —el mismo error otra vez, con el mismo mensaje—
  no toca el índice.
- La migración **rellena el índice** para los issues que existían antes. Una
  búsqueda que ignorase en silencio todo lo anterior a la actualización sería peor
  que no tener búsqueda.
- `issues_fts` es una tabla virtual y por tanto **no puede ser `STRICT`**. Es la
  forma del módulo, no una relajación de la convención del esquema.
- Un índice de contenido externo se **corrompe** si se le manda un `'delete'` para
  una fila que nunca insertó. Por eso los tres triggers y el backfill van juntos en
  la misma migración: descubierto midiendo, con una variante del esquema a la que
  le faltaba el trigger de `INSERT`, que produjo un `database disk image is
  malformed` al primer `UPDATE`.

## Medición (2026-08-29)

El índice se escribe dentro de la transacción de ingesta, así que lo que importa no
es el tamaño sino qué cuesta por evento. Aislado —20 000 filas, mejor de tres, misma
máquina que el ADR 001— y comparado contra no tener índice en absoluto:

| Camino de ingesta | sin índice | `unicode61` | `trigram` |
|---|---|---|---|
| Evento repetido, mismo mensaje | 17,4 µs | 23,7 µs | **23,5 µs** |
| Evento repetido, mensaje distinto | 17,1 µs | 78,1 µs | **142,5 µs** |
| Issue nuevo (una vez por issue) | 17,0 µs | 113,8 µs | **289,5 µs** |

La primera fila es la que decide: **en el caso común, trigram no cuesta nada frente
a unicode61**, porque el trigger acotado por columna y por cambio de valor no
reindexa nada cuando el mismo error vuelve con el mismo mensaje. El peor caso —un
mensaje distinto en cada evento, que es lo que produce un mensaje interpolado— son
+64 µs sobre unicode61, un 5 % de los ~1,2 ms que cuesta un evento entero. Crear un
issue son +176 µs, una vez por issue y no por evento.

End to end, con `make bench`: **614,9 ev/s** creando issue y **765,5** acumulando,
dentro del ruido de las corridas con `unicode61` (607–629 y 719–837 en la misma
máquina). El gate está en 150 y quedan **4,1×**.
