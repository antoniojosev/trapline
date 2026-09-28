# 040 — La documentación de usuario se ejecuta, y el nombre del producto es un marcador

- **Fecha**: 2026-09-20
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

Hasta este punto, todo lo que este producto afirma de sí mismo lo comprueba un
gate. El throughput es `make bench`; la huella es `scripts/footprint.sh`; que
un SDK oficial funcione es `scripts/compat.sh` con el SDK de verdad; que la API
esté congelada es `TestRoutesMatchOpenAPI` recorriendo el documento y la tabla
de rutas en las dos direcciones. La regla escrita en el Makefile —si `make
check` pasa, CI pasa, porque cualquier otra cosa vuelve los gates consultivos,
que es lo mismo que no tenerlos— se aplicó a todo menos a una superficie: la
documentación.

Y la documentación es donde el fallo es más barato de cometer y más caro de
descubrir. Un comando de ejemplo con un flag que se renombró hace dos meses no
rompe ningún build, no aparece en ninguna métrica, y se descubre cuando alguien
que acaba de instalar el producto lo copia, lo pega y obtiene un error de uso.
Ese alguien no abre un issue: se va. Un `README` es la primera superficie que
alguien ejecuta y la única que nadie prueba.

Hay además un segundo problema, específico de este repositorio y con fecha de
caducidad. El nombre del producto está deliberadamente diferido
(`NAME-PLACEHOLDER.md`) y `errtrack` es un placeholder que nadie debe tratar
como definitivo. Pero la documentación de usuario que se escribe ahora está
llena de invocaciones del comando, y cada una es un sitio donde el nombre se
fija por costumbre antes de que exista la decisión. Escribirlas con el
placeholder es exactamente cómo un nombre provisional deja de serlo.

Las dos cosas chocan: un ejemplo con un marcador no se puede ejecutar, y un
ejemplo que se ejecuta fija el nombre.

## Decisión

### Un bloque marcado `sh test` es un test

Los bloques de código de la documentación de usuario se marcan de dos formas, y
la marca significa algo:

- ```` ```sh ````  — ilustrativo. Arrancar un servidor, `docker compose`,
  `curl | sh`, un DSN con un dominio de ejemplo. Nadie lo ejecuta.
- ```` ```sh test ```` — **ejecutable**. `scripts/docs.sh` levanta un servidor
  de verdad, lo siembra con un proyecto y un error, y corre el bloque contra
  él. Si sale distinto de cero, el gate falla y nombra el fichero, el bloque y
  la línea.

La división no es por comodidad: es la frontera entre lo que este repositorio
puede comprobar y lo que no. Un `curl -fsSL https://<host>/install.sh | sh`
depende de una release publicada y de una red, y marcarlo ejecutable sería
mentir sobre lo que el gate demuestra. Que la distinción sea visible en el
propio fichero es lo que impide que esa mentira se cuele por descuido.

Cada bloque corre en su propio shell con `set -euo pipefail`, en un directorio
temporal, contra el mismo servidor. Que compartan servidor y no shell es
deliberado: el estado que un lector tiene al llegar a un ejemplo es el del
producto, no el de las variables que exportó el ejemplo anterior. Un bloque que
necesita una variable la crea.

### El gate también comprueba los enlaces

Un enlace interno roto es la otra mitad del mismo problema. `scripts/docs.sh`
recorre todos los `.md` del repositorio y `docs/agents/llms.txt`, resuelve cada
enlace relativo contra el fichero que lo contiene, y falla si el destino no
existe o si el ancla no corresponde a ningún encabezado. Los enlaces que salen
del árbol —`../../security/advisories/new`, que resuelve en GitHub y no en el
disco— se saltan explícitamente, porque comprobarlos aquí sería comprobar otra
cosa.

### `{{NAME}}` es el nombre del producto hasta que haya uno

La documentación de usuario escribe `{{NAME}}` donde iría el nombre del
producto o el de su comando. `scripts/docs.sh` lo sustituye por `errtrack`
antes de ejecutar un bloque, así que el marcador **no** cuesta ejecutabilidad:
lo que el gate corre es lo que el lector leerá cuando el nombre exista.

Lo que **no** lleva marcador son las variables de entorno (`ERRTRACK_URL`,
`ERRTRACK_TOKEN`, `ERRTRACK_DB`) ni las cabeceras (`X-Errtrack-Request`). Esos
nombres viven en el código Go y en el contrato con clientes que ya existen; la
lista de renombrado de `NAME-PLACEHOLDER.md` no los incluye, y añadir un
segundo marcador para algo que esa lista no toca sería inventar trabajo para el
día del rename. Cuando se decida si se renombran, la decisión es suya y va en
su propio ADR.

**Consecuencia operativa, y es la importante**: el checkpoint de rename tiene
un paso más del que `NAME-PLACEHOLDER.md` enumera hoy —sustituir `{{NAME}}` en
la documentación— y ese fichero no lo edita el gate de documentación.
Queda dicho aquí.

### El gate corre en la PR

Tarda segundos, no necesita contenedores ni red, y lo que protege es la
primera impresión de alguien que todavía no ha decidido si instalar esto.
Descubrirlo a la mañana siguiente no serviría de nada distinto.

## Consecuencias

- **La documentación deja de poder envejecer en silencio.** Renombrar un flag
  rompe el build del mismo modo que renombrarlo en el OpenAPI, que es
  exactamente lo que se quería.
- **Escribir documentación cuesta un poco más.** Un ejemplo ejecutable tiene
  que funcionar contra un servidor recién sembrado: sin ficheros que no
  existen, sin ids inventados, sin estado de un ejemplo anterior. Ese coste es
  el que compra la garantía.
- **El gate siembra un estado concreto** —un proyecto, un error con release y
  stacktrace, un token— y los ejemplos dependen de él. Si la documentación
  necesita más estado, se siembra en el script y se dice ahí por qué; un
  ejemplo que sólo funciona con datos que el lector no tiene es otra forma de
  estar roto.
- **No se comprueba la prosa.** Que un comando corra no dice que su explicación
  sea cierta. Este gate cubre la mitad mecánica, que es la que se rompe sola.
- **Las URLs externas no se comprueban.** Un gate que sale a la red se cae por
  razones que no son suyas y acaba desactivado, que es peor que no tenerlo
  (ADR 002, la misma razón de los dos tiers de la matriz de compatibilidad).
- **ADR fuera del bloque reservado.** La numeración de ADRs
  reserva 030–039 para decisiones no anticipadas, y esas diez ya estaban
  usadas cuando se tomó ésta. Se toma la 040 y se anota, porque perder la
  decisión era la única alternativa.
