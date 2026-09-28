# 033 — Los timestamps se guardan con ancho fijo

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Este ADR corrige una decisión que ya se había tomado, con una justificación que
era falsa. `internal/adapters/sqlite/time.go` guardaba los instantes como TEXT
con `time.RFC3339Nano`, y el comentario que la sostenía decía:

> Timestamps are stored as TEXT in this layout because it **sorts
> lexicographically in the same order it sorts chronologically**, which lets
> SQLite range-scan an index on a text column.

La primera mitad de la frase es la razón correcta para no usar enteros unix: una
fila que un humano lee tiene que poder decir qué dice, y SQLite recorre por
rango un índice sobre texto igual que sobre un entero. Eso sigue en pie. Lo que
no se cumple es la propiedad de la que todo lo demás cuelga.

`RFC3339Nano` **suprime los ceros finales de la fracción**. Un instante que cae
en un segundo exacto se escribe sin fracción ninguna, y `.` (0x2E) es menor que
`Z` (0x5A) en ASCII. Con lo cual:

```
orden cronológico:  10:00:00Z      10:00:00.001Z  10:00:00.5Z  10:00:01Z
orden de SQLite:    10:00:00.001Z  10:00:00.5Z    10:00:00Z    10:00:01Z
```

No es una curiosidad: los segundos exactos son lo que manda cualquier SDK que
redondee, y un corte de retención es una ventana de días enteros restada a un
instante, que también cae en segundo exacto muy a menudo.

De esa comparación textual dependen tres cosas:

1. **El `ORDER BY last_seen DESC` de la pantalla principal.** Un issue visto en
   un segundo exacto se hunde por debajo de otro medio segundo más viejo.
2. **El cursor keyset de paginación** (`last_seen < ? OR (last_seen = ? AND id <
   ?)`), que corta la lista por el mismo criterio y por tanto se salta filas o
   las repite alrededor de un segundo exacto.
3. **El `WHERE received_at < ?` de la barrida de retención.**

El tercero es el caro y es el que decide este ADR. Verificado con un test antes
del arreglo: con corte en `10:00:00Z`, un evento en `10:00:00.5Z` —medio segundo
**dentro** de la ventana que se prometió conservar— se considera anterior al
corte y **se borra**. No hay síntoma después: la fila deja de existir. Es pérdida
silenciosa de datos en un producto cuyo único trabajo es no perder errores.

Además hay una versión menos evidente de lo mismo en `ensureRelease`, que
ensancha `first_event_at` y `last_event_at` con `MIN()` y `MAX()` sobre ese texto
en cada evento que nombra una release.

## Decisión

**El layout de almacenamiento pasa a ser de ancho fijo: la fracción está siempre
presente y siempre tiene nueve dígitos.**

```go
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"
```

Con el mismo número de caracteres en todos los valores, comparar dos de estas
cadenas byte a byte *es* comparar los instantes que nombran, para todo par. La
propiedad deja de ser una afirmación sobre un formato y pasa a ser una
consecuencia de su forma.

Tres piezas más, que son parte de la decisión y no detalles de implementación:

- **Migración 0010** reescribe todas las columnas de tiempo del esquema. El
  inventario se hizo recorriendo las migraciones, no de memoria: 22 columnas en
  11 tablas, `schema_migrations.applied_at` incluida. Las columnas `hour` de los
  agregados (`issue_hourly`, `project_hourly`, `project_hourly_dims`) se
  revisaron y quedan fuera: son `domain.HourLayout` (`YYYY-MM-DDTHH`), sin
  fracción y ya de ancho fijo.
- **`parseTime` sigue leyendo el formato viejo.** Un binario abre y migra la
  base en la misma llamada, y una base restaurada de un backup antiguo tiene
  filas sin reescribir. Sólo se lee; escribir hay una sola forma. Lo que no
  entiende ninguno de los dos layouts falla nombrando ambos.
- **`formatTime` acota al rango representable** (años 1 a 9999). Fuera de ahí Go
  escribe cinco dígitos de año o un `-` delante, que es exactamente el fallo que
  este documento arregla, reintroducido por el otro extremo. El `timestamp` del
  protocolo puede llegar como número, y `1e17` cae tres mil millones de años
  fuera; `decodeTimestamp` lo trata como cualquier otro timestamp inservible y
  usa el reloj del servidor, para que el acotado no llegue a dispararse en la
  práctica.

## Consecuencias

- **La columna crece.** Un instante pasa de 20-24 caracteres a 30 fijos. Se
  escribe en cada evento (`received_at` y `occurred_at`) y en el update del issue
  (`first_seen`, `last_seen`). El coste medido está en la sección "Medición".
- **La migración es una reescritura de tabla completa, `events` incluida.** No
  hay versión más barata: las filas que hay que cambiar son justamente las que no
  tienen fracción o la tienen corta, y distinguirlas cuesta el mismo recorrido
  que reescribirlas. Corre una sola vez, dentro de la transacción de la
  migración, sobre una base que todavía no sirve peticiones.
- **La conversión es idempotente**: un valor que ya está en el layout nuevo se
  convierte en sí mismo, y un valor que no tiene forma de timestamp se deja
  intacto en vez de mutilarse. Un restore que reproduzca migraciones, o un
  operador que la vuelva a lanzar a mano, no rompe nada.
- **Los cursores emitidos antes de la migración dejan de casar.** Un cursor
  codifica `last_seen` como texto; uno viejo comparado contra columnas nuevas
  corta en el sitio equivocado. Son opacos, viven lo que una sesión de listado y
  el producto no tiene instalaciones: no se versiona el cursor. Si algún día hay
  que hacerlo, el sitio es `encodeCursor`.
- **La propiedad queda como test, no como comentario.** Es la lección de haberla
  escrito una vez como afirmación: `TestStoredTimeOrdersLexicographically`
  compara todos los pares de una tabla de casos (segundo exacto, un dígito de
  fracción, nanosegundos, medianoche, fin de día, fin de año, los extremos del
  rango) y `TestStoredTimeOrdersRandomInstants` hace lo mismo sobre 20 000 pares
  generados con semilla fija y sesgados hacia segundos exactos.
  `TestRFC3339NanoDoesNotOrder` conserva el bug como aserción: si un Go futuro
  dejara de recortar la fracción, el que falla avisa.
  `TestTimeColumnsInventoryIsComplete` recorre el esquema y falla si aparece una
  columna TEXT con pinta de instante que la migración 0010 no reescribe, que es
  la única forma de que el inventario no se quede viejo.

## Alternativas descartadas

- **Guardar enteros unix (nanosegundos)**: ordena bien y ocupa menos, pero hace
  ilegible cada fila que alguien mire con `sqlite3` y cada consulta escrita a
  mano, que es la razón original —y buena— de que esto sea TEXT. Además exigiría
  la misma migración, y una conversión en cada lectura y cada escritura.
- **Dejar el formato y arreglar cada consulta** (comparar `datetime(col)` o
  `julianday(col)` en vez del texto): rompe el uso de los índices, convierte cada
  `ORDER BY` en un cálculo por fila, y hay que acertar en todos los sitios —hoy y
  cada vez que se escriba una consulta nueva—. La forma del dato es el sitio
  donde esto se arregla una vez.
- **Normalizar sólo `received_at` y `last_seen`**, que son las columnas donde el
  fallo duele: deja el esquema con dos convenciones y la siguiente columna de
  tiempo se escribe en la equivocada.
- **Una fracción de milisegundos (tres dígitos)**: también es de ancho fijo y
  ocupa seis caracteres menos, pero tira precisión que el SDK sí manda y que dos
  eventos del mismo milisegundo necesitan para ordenarse entre sí.

## Medición

Ver la sección "Medición" anotada en el ADR 001; `make bench` reproduce
la cifra.
