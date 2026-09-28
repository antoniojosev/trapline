# 013 — Compatibilidad con `sentry-cli`: superficie `/api/0/` emulada a partir de tráfico grabado

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Una release sólo vale lo que vale el momento en que se registra. El producto ya
sabe qué release trajo un error (ADR 012), pero eso exige que alguien le diga
que la release existe, y quien lo dice es el pipeline de despliegue. Si
integrar este servidor en un pipeline significa escribir `curl` a mano, la
funcionalidad existe para quien tenga tiempo de cablearla y para nadie más.

`sentry-cli` es la herramienta que ya está en esos pipelines. Cambiar de
servidor debería ser cambiar `SENTRY_URL`, igual que migrar un SDK es cambiar
el DSN (ADR 002).

El problema es saber qué habla realmente esa herramienta. La documentación
pública de esa API es parcial, va por detrás del binario, y el binario manda
cosas que ninguna página menciona. Este proyecto ya tiene la cicatriz:
`@sentry/node` no manda ninguna cabecera de autenticación —sólo `?sentry_key=`
en la query— y un servidor construido a partir del `X-Sentry-Auth` documentado
habría rechazado a todas las instalaciones de Node mientras sus propios tests
seguían en verde. Ese fallo no se ve desde dentro: los tests prueban la idea
que este proyecto tiene del cliente, que es exactamente lo que está en duda.

## Decisión

### 1. Grabar antes de implementar

Se levanta un servidor grabador (`compat/sentry-cli/recorder/`, módulo Go
aparte para que un servidor HTTP de desarrollo no entre nunca en el grafo de
dependencias del producto), se ejecuta el `sentry-cli` **real y pinneado** en
contenedor contra él, y cada petición se persiste —método, ruta, query,
cabeceras relevantes, cuerpo— como fixture **commiteada** en
`compat/sentry-cli/fixtures/<versión>/`.

La implementación se escribe a partir de esas fixtures y se prueba **contra las
fixtures y contra la herramienta real**. Son dos afirmaciones distintas: las
fixtures fijan lo que el cliente *manda*; sólo el cliente puede demostrar que
acepta lo que este servidor *responde*, y una respuesta que no puede parsear
termina un despliegue a mitad, con la release ya creada y sus commits perdidos.

Cuando la versión pinneada (`compat/sentry-cli/version.env`) cambie, se
regraba y **se lee el diff**: una fixture que cambió es un cambio de protocolo
que alguien tiene que mirar.

### 2. Qué reveló la grabación

Lo que sigue no estaba en ninguna página y contradice lo que se habría escrito
de memoria:

- **El namespace cambia dentro del mismo comando.** Crear una release va al
  *proyecto* (`POST /api/0/projects/{org}/{project}/releases/`) y fijar sus
  commits va a la *organización* (`PUT /api/0/organizations/{org}/releases/
  {version}/`). Un servidor que implementara sólo uno fallaría a mitad del
  pipeline, con todo ya creado.
- **`set-commits --local` hace cuatro llamadas, no una**: lista los
  repositorios de la organización, crea la release, pregunta cuál fue la
  anterior con commits, y sólo entonces manda el conjunto.
- **Dos de esas respuestas tienen que ser un array JSON.** Contestar `{}` —el
  stub obvio— detiene la herramienta con `invalid type: map, expected a
  sequence`.
- **`finalize` rechaza un objeto de release sin `version`**, así que un `200
  {}` vacío no es respuesta válida para ninguna de estas rutas.
- **`deploys new` es de organización**, y exige `--release` o `SENTRY_RELEASE`;
  no deduce la release de nada.
- **Toda ruta lleva barra final** y **la versión va sin escapar en el path**
  (`app@1.0.0`).
- **Autenticación**: `Authorization: Bearer …` en todas las peticiones, sin
  excepción. Es lo que confirma que el token propio (`ek_…`) puesto en
  `SENTRY_AUTH_TOKEN` es suficiente.

### 3. Alcance de la emulación

Se emula **sólo** el subconjunto que `sentry-cli` invoca para
`releases new | set-commits --local | finalize | deploys new`. No se promete
compatibilidad con el resto de esa API, y una ruta desconocida bajo `/api/0/`
lo dice así, en la forma de error que el cliente sabe leer (`detail`).
`sourcemaps upload` y la subida de ficheros de release son la subida de
artefactos, medida más abajo.

### 4. Identidad: organización, proyecto, credencial

- **El slug de organización se ignora.** Esta es una instalación de una sola
  organización: no hay nada que el segmento pueda seleccionar, y rechazar un
  valor obligaría a cada usuario a descubrir un nombre que este producto nunca
  le pidió.
- **El proyecto se acepta por slug o por id numérico.** El id se resuelve
  primero, y no es arbitrario: es el identificador que el protocolo ya impone
  a toda instalación —va en el path de ingesta de cada DSN— así que es el que
  no se le puede quitar a nadie. Leer un número como otra cosa haría que un id
  a veces direccionara otro proyecto.
- **`projects.slug` es columna nueva** (migración 0012), derivada del nombre y
  única. La derivación es una función pura del dominio (`domain.Slugify`) y el
  almacén es quien la desambigua, porque la unicidad es un hecho del almacén y
  no de una cadena. Un slug que sea sólo dígitos se descarta por la misma razón
  que el id se resuelve primero: nunca podría resolverse como slug.
- **La credencial es el bearer propio**, y **sólo** el bearer: esta superficie
  no acepta la cookie de sesión.

### 5. Por qué `/api/0/` queda fuera del guardia CSRF

La cabecera `X-Errtrack-Request` defiende una credencial **ambiente**: a un
navegador se le puede engañar para que reenvíe una cookie que ya tiene, así que
una petición que cambia estado autenticada por cookie tiene que demostrar que
fue deliberada. Aquí no hay cookie: `requireToken` exige bearer y no mira otra
cosa. Un bearer no es ambiente —una página no puede hacer que el navegador lo
adjunte— así que no hay nada que falsificar, y exigir una cabecera que
`sentry-cli` no ha oído nombrar rechazaría al único cliente para el que existe
la superficie. Ese fallo habría sido total y silencioso: todos los tests de
este paquete en verde y ningún pipeline capaz de desplegar. Es exactamente la
forma del bug de CORS que encontró la suite de navegador (ADR 002).

El despacho vive en `apiNamespace` (`httpapi/json.go`), y entra **delante** del
reparto que ya había sin tocarlo: cero no es un entero positivo, así que
`/api/0/` nunca pudo ser una ruta de ingesta, y hasta ahora caía en la API del
panel y contestaba 404.

### 6. Una llamada de organización se resuelve por la versión

Fijar commits y registrar un deploy no nombran proyecto alguno. Con una sola
organización, lo único que queda para resolver la llamada es la propia versión:
`usecase.Releases.ProjectsWithVersion` devuelve **todos** los proyectos que
tienen esa release y el handler aplica el cambio a todos. En la API que se
emula una release pertenece a la organización y abarca varios proyectos, así
que un handler que eligiera uno perdería la escritura de los demás en silencio.

## Consecuencias

- **Los campos desconocidos se ignoran en esta superficie**, al revés que en la
  API propia, que los rechaza. Es la misma tolerancia del parser de envelopes
  (ADR 002): los campos de este cable los elige otro y ganan miembros entre
  versiones de su herramienta —`dateStarted` en un create, `refs` en un
  conjunto de commits, lo que venga después— y rechazar el primero que no se
  reconoce rompería por una actualización en la que nadie de aquí participó.
- **El gate es nocturno, no de pull request.** Tira de dos imágenes y conduce a
  un cliente real por un pipeline de cuatro comandos. La mitad rápida —las
  peticiones grabadas reproducidas contra el handler— corre en `make check` con
  todo lo demás, así que un cambio que rompa la forma del cable se ve en la
  revisión igual.
- **`previous-with-commits` contesta 404 y no la verdad.** La respuesta honesta
  exigiría un orden sobre las releases *que llevan commits*, que este servidor
  sabe calcular (ADR 012) pero no tiene consulta para ello. El 404 pone a la
  herramienta en la rama en que deriva el rango del checkout que tiene delante
  —lo mismo que hace un primer despliegue— y manda de más en vez de mandar la
  ventana equivocada. Cuando *suspect commits* necesite esa consulta,
  esta ruta puede dejar de mentir por omisión.
- **`repos` contesta lista vacía, y eso es la verdad**: este producto no se
  integra con ningún alojamiento de código, guarda los commits que le entrega
  una herramienta de despliegue y no clona nada. La lista vacía es además lo
  que pone a `set-commits --local` en la rama local.
- **Un slug migrado puede no ser el que la instalación esperaba.** La
  derivación en SQL de la migración 0012 conoce menos que la de Go, y cualquier
  nombre que no reduzca a `[a-z0-9-]` cae al respaldo `project-<id>`. Es
  visible en `projects list` y en el panel, que es donde alguien va a buscarlo;
  no hay operación para renombrarlo, y ese es trabajo pendiente declarado.
- **No hay compatibilidad prometida más allá de estas rutas.** Es una decisión
  de alcance, no una promesa a medias: quien pida otra cosa recibe un 404 que
  explica qué cubre esta superficie.

## Medición (2026-08-29)

La grabación son **siete peticiones** para los cuatro comandos:

| # | | |
|---|---|---|
| 1 | `POST` | `/api/0/projects/{org}/{project}/releases/` (`releases new`) |
| 2 | `GET` | `/api/0/organizations/{org}/repos/?cursor=` |
| 3 | `POST` | `/api/0/projects/{org}/{project}/releases/` (otra vez, desde `set-commits`) |
| 4 | `GET` | `/api/0/organizations/{org}/releases/{version}/previous-with-commits/` |
| 5 | `PUT` | `/api/0/organizations/{org}/releases/{version}/` (el conjunto de commits) |
| 6 | `PUT` | `/api/0/projects/{org}/{project}/releases/{version}/` (`finalize`) |
| 7 | `POST` | `/api/0/organizations/{org}/releases/{version}/deploys/` |

Dos namespaces, dos `PUT` con la misma forma de ruta y significados distintos,
y un `POST` repetido que obliga a que crear una release dos veces no sea un
conflicto. Nada de eso se habría escrito de memoria correctamente.

## Medición (2026-08-29) — la mitad de source maps

El §3 de arriba dejaba fuera `sourcemaps upload` y la subida de ficheros de
release. Ya están grabados —cuatro flujos más, quince ficheros— y ya están
implementados. La emulación
crece con seis rutas:

| # | | |
|---|---|---|
| 1 | `GET`  | `/api/0/organizations/{org}/chunk-upload/` (capacidades) |
| 2 | `POST` | `/api/0/organizations/{org}/chunk-upload/` (los chunks) |
| 3 | `POST` | `/api/0/organizations/{org}/artifactbundle/assemble/` |
| 4 | `POST` | `/api/0/organizations/{org}/releases/{version}/assemble/` (el respaldo) |
| 5 | `GET`  | `/api/0/projects/{org}/{project}/releases/{version}/files/` (deduplicación) |
| 6 | `POST` | `/api/0/projects/{org}/{project}/releases/{version}/files/` (el camino viejo) |

Lo que la grabación añadió a lo que ya decía este ADR, y que tampoco se habría
escrito de memoria:

- **Aquí es la RESPUESTA la que decide el protocolo, no la petición.** El campo
  `accept` del documento de capacidades cambia la ruta entera que toma el mismo
  comando, sin avisar. Está en ADR 018, anexo §3.
- **Los códigos de estado dejan de ser un canal a mitad del flujo.** En el
  `POST` de chunks `200`, `201` y `202` valen igual y un `500` corta; en el
  ensamblado el código no se lee como error en absoluto — un `413` sale como
  *"unknown error"*, y el fallo viaja dentro del cuerpo (ADR 038).
- **Hay una ruta que sólo existe para no ser un 404.** La consulta de
  deduplicación se emite siempre que hay `--release` y su respuesta da igual —
  medido con `[]`, con la lista real y con `{}`. Está en el camino feliz, así
  que lo único que importa de ella es que exista.
- **Y hay una respuesta que este servidor se niega a dar antes de tiempo.**
  `{"state":"ok"}` se cree a pies juntillas: contestarlo sin tener el bundle
  termina el comando con éxito y sin ficheros. Es el mismo tipo de fallo total
  y silencioso que el header de autenticación de `@sentry/node`, y por eso el
  gate `scripts/sourcemaps.sh` incluye un caso negativo que lo demuestra.
