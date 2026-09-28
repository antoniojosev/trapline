# 022 — MCP como cuarto cliente: dos transportes, una tabla, y el bundle que leen

- **Fecha**: 2026-09-20
- **Estado**: Aceptada

## Contexto

El argumento del producto no es que tenga menos features que el incumbente por
menos dinero. Es que **un agente lo opera**: el error llega, el agente lo lee,
parchea el código, corre los tests y lo marca resuelto para la próxima release.
ADR 006 lo dejó escrito desde el principio —una API, cuatro clientes— y hasta
hoy sólo existían tres. El cuarto es éste.

Un agente que hoy quiera arreglar un bug con la API REST tal cual hace cinco
llamadas: el issue, sus eventos, sus tags, sus suspect commits y su historia.
Cada una es un viaje de ida y vuelta que paga dos veces —en latencia y en
contexto gastado decidiendo qué preguntar después— y las cinco respuestas son
JSON, del que tiene que volver a deducir la forma en cada llamada. El resultado
es que la parte más cara de arreglar un error automáticamente no es arreglarlo:
es reunir lo que hay que leer antes.

Hay además una trampa concreta en la que caen los servidores MCP, y conviene
nombrarla antes de decidir nada: al estar dentro del proceso, es tentador que
las herramientas llamen directamente a los casos de uso. Eso convierte el
servidor MCP en **una segunda implementación de cada endpoint**, con su propia
idea de qué permite cada scope. La mitad que nadie nota que ha derivado es
justamente ésa, y se descubre el día en que un token de lectura resuelve un
issue.

## Decisión

### Una herramienta MCP es una llamada a la API REST, no un atajo hacia dentro

La tabla de herramientas (`internal/adapters/mcp/tools.go`) es un dato: cada
entrada declara un nombre, una descripción, un esquema de argumentos y **la
petición que construye** — método y ruta bajo `/api/v1/`. Quién lleva esa
petición es lo único que distingue a los dos transportes:

- `errtrack mcp` (stdio) la manda por la red a `ERRTRACK_URL` con
  `ERRTRACK_TOKEN`, exactamente como hace la CLI;
- `POST /mcp` la entrega al router de este mismo proceso, en memoria, con **la
  credencial con la que llegó la petición MCP**.

De ahí se siguen las tres propiedades que importan. Una herramienta no puede
hacer nada que la API no pueda. No puede saltarse una comprobación de scope:
`resolve_issue` se rechaza para un token sin `projects:write` por la misma
línea de `routes.go` que rechaza `POST …/status`. Y no puede responder algo
distinto de lo que responde el endpoint, porque es el endpoint.

Que el transporte HTTP vuelva a entrar por su propio router en vez de llamar a
un caso de uso es deliberado y tiene un coste real —una petición sintética por
llamada— que se paga a cambio de que el guardia sea el de verdad.

### stdio es un cliente REST, no un modo local

`errtrack mcp` no abre la base de datos. Podría: está en el mismo binario. No lo
hace por dos razones. La primera es que así sirve a una instalación que corre en
otra máquina, que es el caso normal —el agente está en el portátil y el servidor
en un VPS—. La segunda es la de arriba: un modo local sería la segunda
implementación por otro camino.

### El bundle: un documento, no un registro

`GET /projects/{id}/issues/{issueID}/bundle` devuelve `text/markdown`: el
stacktrace symbolicado con contexto de código y el frame minificado del que
salió (ADR 018), los breadcrumbs, los tags y contextos agregados, el ciclo de
vida de release (ADR 012), la frecuencia de 24 h y 14 d leída de los agregados
horarios (ADR 010) y los commits sospechosos (ADR 019). En una petición.

Markdown y no JSON porque el lector es un modelo de lenguaje. Un documento con
encabezados le cuesta menos que un registro cuya forma tiene que volver a
deducir de nombres de campo, y puede decir **en palabras** lo que un número no
dice solo: que «resuelto en la próxima release» significa que los eventos que
siguen llegando son las máquinas sin redesplegar y no una regresión. El JSON
sigue estando y no se ha movido: es `GET …/issues/{issueID}`.

Está bajo el proyecto, como todo lo demás que se pregunta sobre un issue, y no
en un `/issues/{id}/bundle` de primer nivel: el id de un issue sólo es único
dentro de su proyecto en el resto de esta API, y un endpoint que lo aceptara
suelto sería el único sitio donde adivinar un número alcanza los datos de otro.

### El render es determinista

El mismo issue produce los mismos bytes. Dos consumidores dependen de ello: la
caché de prompt de un agente está indexada por el texto, y comparar dos bundles
es como alguien comprueba si un issue cambió entre dos lecturas. En la práctica
significa que **todo mapa se ordena antes de escribirse** y que el render no
lee ningún reloj — las ventanas las nombra quien llama y las fechas vienen de
lo guardado. Hay fixtures dorados, por el mismo motivo que los tiene el digest
(ADR 035): sin ellos, «determinista» es una intención.

Y todo valor que viene de un evento pasa por una función que colapsa saltos de
línea. Un evento lo escribe quien tenga un DSN —para un SDK de navegador, todo
el mundo—, y un `\n` en el mensaje de una excepción terminaría su línea y
dejaría al resto de la cadena empezar un encabezado propio, en un documento
sobre el que un agente está a punto de actuar.

### `/mcp` va fuera de `/api/v1/`, y fuera del guardia CSRF

Fuera del prefijo versionado porque la URL de un cliente MCP vive en un fichero
de configuración que un cambio de versión no podría reescribir, y porque `/mcp`
es la convención de ese protocolo y no una ruta nuestra — la misma razón por la
que la ruta de ingesta y la de `/ping/` tampoco se versionan.

Fuera de `requireCSRFHeader` porque esa defensa protege una **credencial
ambiental** —una cookie de sesión— y aquí no se acepta ninguna: la ruta
autentica sólo con `Authorization: Bearer` (`requireToken`). Exigir una
cabecera inventada por este producto rechazaría a todos los clientes MCP que
existen, y el fallo sería total y silencioso, que es exactamente la forma del
bug de CORS que encontró la suite de navegador.

Aun así está en la tabla de rutas no versionadas, así que el gate de OpenAPI la
ve y `docs/api/openapi.yaml` la describe. Una superficie que el gate no ve es
una superficie que nadie escribió.

### Transporte HTTP sin estado

`POST /mcp` responde `application/json`, sin `Mcp-Session-Id` y sin flujo de
eventos; `GET` y `DELETE` sobre esa ruta responden 405. Ninguna de estas diez
herramientas hace streaming ni llama de vuelta al cliente, así que una sesión
con estado añadiría un almacén que caduca y dos métodos más sobre la misma ruta
a cambio de nada. Además es lo que permite que haya **un solo** `mcp.Server`
compartido: la credencial de cada llamada sale de la petición que la trajo, no
del objeto.

### Se usa el SDK oficial, medido

`github.com/modelcontextprotocol/go-sdk` está permitido si el binario se
queda en ≤30 MB. Se midió, y entra:

| | tamaño |
|---|---|
| binario antes de MCP | 16,10 MB |
| binario con el SDK y todo MCP | 18,13 MB |
| presupuesto (ADR 009, Makefile) | 30 MB |

El coste marginal del SDK y sus nueve dependencias transitivas es ~1,7 MB,
`govulncheck` sale limpio y todas son Go puro, así que las cuatro condiciones
para admitir una dependencia (ADR 009) se cumplen. Escribir el subconjunto JSON-RPC a mano habría sido
posible, pero el transporte streamable HTTP no es sólo JSON-RPC —negociación de
versión de protocolo, `initialize`, cabeceras de sesión, formas de error— y
equivocarse ahí se manifiesta como un cliente que no conecta y no dice por qué.

Lo que **no** se usa del SDK es su `AddTool` genérico, que deduce el esquema de
un tipo Go. Los esquemas se escriben a mano porque son la única documentación
que el agente llega a leer: un esquema deducido describiría `in_next_release`
como `bool`, y ese campo necesita una frase.

### Las diez herramientas, y por qué tres verbos en vez de uno

`list_projects`, `list_issues`, `get_issue`, `get_issue_bundle`,
`resolve_issue`, `ignore_issue`, `reopen_issue`, `get_release_health`,
`query_stats`, `list_transactions`.

Las tres de triaje pegan contra el mismo endpoint, que toma un estado. Son tres
herramientas y no una `set_issue_status` porque un modelo tendría que adivinar
el vocabulario, y un estado adivinado es un 400 del que luego hay que
recuperarse.

`project` acepta el id numérico **o el slug**. Resolver un slug cuesta una
llamada a `/projects` con la credencial de quien pregunta —así que no es una
forma de saber que un proyecto existe sin poder leerlo— y ahorra el paso que el
agente daría si no: listar, leer un número del JSON, y entonces sí preguntar.

### Un fallo es un error de herramienta, nunca de protocolo

Un argumento malo, un scope que falta y un id que no existe vuelven como
`isError` con una frase. Un error de protocolo aborta el turno del agente;
`isError` le cuesta un reintento. La frase **nombra el estado HTTP**, porque
403 y 404 llevan a movimientos opuestos y «falló» lleva a repetir el mismo.

## Consecuencias

- Paridad ADR 006 completa para el bundle: `GET …/bundle`, `errtrack issues
  bundle`, el botón «Copy for an agent» del detalle del issue, y la herramienta
  `get_issue_bundle`. Los cuatro devuelven el mismo documento; el gate compara
  los bytes.
- `scripts/mcp.sh` levanta un servidor, corre los dos transportes a la vez con
  un cliente MCP real (`compat/mcp`, SDK oficial), exige que `tools/list` sea
  idéntico en ambos, compara los dos bundles contra el de REST y termina con la
  negativa a un token de sólo lectura. Corre en la PR: tarda ~20 s y no
  necesita contenedores.
- El binario crece 2,03 MB (16,10 → 18,13). El presupuesto de 30 MB sigue con
  margen, pero es el primer subsistema que gasta más de un megabyte de golpe y
  conviene que el siguiente lo sepa.
- `POST /mcp` es la primera ruta no versionada que este producto añade por
  decisión propia (las otras tres las impone un protocolo ajeno o un navegador).
  Queda documentada en el OpenAPI y en la tabla, y el gate la vigila.
- El bundle **no** añade columna, migración ni job. Es una proyección de
  lecturas que ya existían.
- Deuda consciente: el bundle no incluye el waterfall de una transaction lenta
  ni el crash-free de la release. Son la respuesta a otra pregunta —«qué está
  lento», «qué release es la mala»— y hay herramientas propias para ellas
  (`list_transactions`, `get_release_health`). Meterlo todo en un documento lo
  volvería el volcado que nadie lee entero.
