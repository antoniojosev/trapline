# 019 — Suspect commits por intersección de rutas

- **Fecha**: 2026-09-20
- **Estado**: Aceptada

## Contexto

Un issue nuevo aparece media hora después de un deploy. La pregunta que se hace
la persona que lo abre no es «¿qué excepción es?» —eso ya lo dice el título—
sino **«¿qué cambió?»**. Hoy el producto puede responder «apareció por primera
vez en `app@1.4.0`» (ADR 012) y puede enseñar los commits de esa release
(ADR 012, `set-commits`), pero deja el cruce final —cuál de esos veinte commits
tocó el fichero que aparece en el stacktrace— a que alguien lo haga con los
ojos.

Ese cruce es barato y es exactamente el dato que el producto ya tiene guardado.
`release_commit_files` existe desde que hay releases con commits precisamente
para esto, y desde que hay source maps un frame de JavaScript resuelto dice `src/checkout.ts` en lugar de
`bundle.min.js` (ADR 018). Las dos mitades están; falta juntarlas.

Lo que **no** se va a hacer, y conviene dejarlo escrito porque es la pendiente
por la que se resbala este tipo de función: no hay integración con GitHub, ni
con GitLab, ni `git blame` remoto, ni modelo de «ownership», ni heurísticas
sobre el tamaño del commit o sobre quién lo escribió. Todo eso son conjeturas
disfrazadas de evidencia, y cada una traería credenciales de terceros a un
producto cuyo argumento es que se despliega con un binario y un fichero.

## Decisión

### Los candidatos son los commits de la release en la que el issue apareció

Para un issue, los candidatos son los commits asociados a su `first_release` —
la release en la que se vio por primera vez, no la última en la que se ha
visto. El cambio que introdujo un bug viajó con su primera aparición; la última
release es simplemente lo que está desplegado ahora. Un `set-commits` envía el
delta desde la release anterior, así que ese conjunto **es** el código que entró
entre la release anterior conocida (exclusive) y ésta (inclusive).

Si el issue no lleva release, si la release no está registrada, o si no tiene
commits asociados, no hay candidatos y se dice por qué (abajo).

### La puntuación

Cruce por **sufijo de ruta** entre `release_commit_files.path` y el fichero que
nombra cada frame, con los frames en orden de llamada (el que lanzó primero):

- Un frame aporta `1 / (profundidad + 1)`. El frame que lanzó pesa el doble que
  su llamante y cuatro veces más que el tercero. La caída es rápida porque la
  llamada que falló es más probablemente la que se rompió que el framework que
  la llamó, y lo bastante lenta como para que un commit que toca cuatro frames
  intermedios gane a uno que toca un único frame profundo.
- **Un frame cuenta una sola vez por commit**, por muchos de sus ficheros que
  coincidan. Sin esa regla, el commit que tocó un fichero, su test y su snapshot
  le gana al commit que cambió la línea que se rompió, sólo por volumen.
- Empates: primero la coincidencia más específica (más segmentos de ruta
  compartidos), después el commit más reciente, y por último el sha, que no
  decide nada pero hace el orden determinista. Un «sospechoso» que cambia al
  recargar la página no es un sospechoso.
- Se exponen **los tres mejores** y como máximo **tres razones** por cada uno.
  La lista se lee como una acusación y su valor se hunde con su longitud: tres
  commits son una pista, quince son la página de la release otra vez.

### El cruce es «uno es sufijo del otro», y nada más blando

Las dos partes escriben el mismo fichero de formas distintas: el repositorio
dice `src/checkout.ts`; el bundler escribe `webpack:///./src/checkout.ts` en el
mapa; quien sube escribe `~/static/app.js`; el navegador informa
`https://app.example.com/static/app.js?v=8f3a`; una cadena de Windows escribe
barras invertidas. Todas se reducen a segmentos en una función pura
(`domain.pathSegments`) y coinciden si **una ruta es sufijo de la otra**.

`lib/util.ts` y `src/util.ts` comparten nombre de fichero y **no** coinciden.
Puntuar eso como coincidencia parcial es exactamente cómo una lista de
sospechosos se llena de tonterías plausibles, y una lista así se aprende a
ignorar en una semana.

### Frames `in_app`, con una excepción medida

Se consideran los frames `in_app` cuando el evento marca alguno. Cuando ninguno
lo hace —que es lo que envían varios SDK de navegador, y el navegador es la
plataforma para la que existe toda esta fase— se consideran todos. La
alternativa, aplicar la regla al pie de la letra, sería no atribuir nada
precisamente donde más falta hace; y el cruce por sufijo es lo bastante estricto
como para que una ruta de `node_modules` no coincida con nada de un repositorio
de todas formas.

### Sin `patch_set` se listan los commits, sin marcar, con aviso

Es el caso explícito del plan y es el frecuente: `sentry-cli releases
set-commits` **sin `--local`** no envía rutas. La respuesta entonces es la lista
de commits candidatos y un aviso que nombra qué cambiar. Lo mismo para los otros
tres finales vacíos, cada uno con su frase:

| Situación | Lo que se dice |
|---|---|
| El issue no lleva release | pon `release` en el init del SDK |
| La release no está registrada | no hay conjunto de commits donde mirar |
| La release no tiene commits | mándalos desde el pipeline, con el comando |
| Los commits llegaron sin rutas | vuelve a mandarlos con `-repo` / `--local` |
| Hay rutas y no coinciden, y los frames siguen minificados | sube los source maps |
| Hay rutas y no coinciden, con frames resueltos | este código no cambió |

Los dos últimos son la misma consulta con respuestas opuestas, y distinguirlos
cuesta un booleano (`symbolicated`, que es «¿algún frame tiene `raw`?»). Sin él,
un front-end sin mapas subidos recibe «ningún commit coincide» y se va a buscar
un fallo en la puntuación en vez de a su pipeline de build.

### Se calcula al preguntar, no se guarda

Las dos mitades de la entrada se mueven: un `set-commits` llega después del
primer error tan a menudo como antes, y la ruta de un frame cambia en el momento
en que alguien sube los source maps que lo resuelven. Un sospechoso guardado
sería la respuesta de un instante pegada a un issue que cambia de opinión.

Es una ruta propia (`GET /projects/{id}/issues/{issueID}/suspects`) y no un
campo del detalle del issue, por lo mismo: lee el conjunto de commits entero de
una release y descomprime un payload guardado, y quien abre un issue para leer
el stacktrace no debe pagar por la conjetura. Además, así un cliente distingue
que ha fallado la conjetura de que ha fallado la página.

### La fuente de rutas es un `git log`, local

`errtrack releases commits -project <id> -version <v> -repo . -from <sha> -to
<sha>` ejecuta `git log --name-status` sobre el checkout que el pipeline ya
tiene delante y sube el mismo payload que `sentry-cli releases set-commits
--local`. Es el único sitio donde esta información es gratis: el código está
ahí, no hace falta credencial de nadie, y no hay que clonar nada.

Un rename se guarda como dos hechos —la ruta nueva añadida y la vieja borrada—
porque un evento anterior al rename todavía nombra el fichero viejo.

## Consecuencias

- `internal/domain/suspect.go` es la puntuación entera, función pura y sin
  reloj. `internal/usecase/suspects.go` junta las tres lecturas (issue,
  release, ocurrencia guardada) y traduce el vacío a una frase accionable.
- Paridad ADR 006: `GET …/issues/{id}/suspects`, `errtrack issues suspects` y la
  sección «Suspect commits» del detalle del issue. MCP cuando exista.
- El coste es una lectura de commits de una release y la decodificación de una
  ocurrencia guardada, ambas fuera del camino de ingesta. No toca `RecordEvent`,
  no añade columna, no añade migración y no añade job.
- La puntuación **no es una probabilidad** y no se presenta como tal. Es un
  orden dentro de una lista, y llamarlo «87 % de confianza» sería inventarse una
  medida que nada aquí mide.
- Un issue cuyo `first_release` cambió de manos —porque alguien borró y
  recreó la release— se queda sin candidatos y lo dice. Es preferible a acusar
  a los commits de otra release.
