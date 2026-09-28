# Las decisiones, en orden de lectura

[`docs/adr/`](../adr/README.md) es la tabla cruda: una decisión por fichero,
numerada por cuándo se tomó e inmutable. Eso está bien para buscar y fatal para
entender — el número 1 y el número 39 hablan de lo mismo, y el 4 y el 22 no
tienen nada que ver.

Esta página es el otro orden: **por la pregunta que responden**. Se lee de
arriba abajo, y cada sección dice qué se decidió y por qué importa antes de
mandarte al fichero.

Antes de esto, [ARCHITECTURE.md](../../ARCHITECTURE.md) — cinco minutos, el
mapa de paquetes y las fronteras. Esto es el porqué de ese mapa.

---

## 1. La forma del producto

Cuatro decisiones que explican todo lo demás. Si sólo lees una sección, ésta.

| ADR | Decisión | Por qué manda |
|---|---|---|
| [001](../adr/0001-sqlite-como-unico-estado.md) | SQLite como único estado | de aquí sale el binario único, y también la regla de oro: **ninguna consulta escanea payloads** |
| [006](../adr/0006-una-api-cuatro-clientes.md) | Una sola API, cuatro clientes | un feature no está terminado hasta estar en REST, CLI, panel y MCP. Es lo que impide que la CLI sea de segunda |
| [005](../adr/0005-toggles-con-backpressure.md) | Toggles con costo cero vía backpressure | apagar un subsistema no cuesta ni ancho de banda: el rate-limit del propio protocolo se lo dice al SDK |
| [009](../adr/0009-sqlite-puro-go-sin-cgo.md) | Driver SQLite puro Go, sin CGO | sin esto no hay cross-compilación, ni imagen `FROM scratch`, ni instalador de un fichero |

## 2. Hablar el protocolo de otro

El producto entero descansa en que cambiar el DSN funcione. Ninguna de estas
tres se implementó leyendo documentación.

| ADR | Decisión |
|---|---|
| [002](../adr/0002-compatibilidad-envelope-sentry.md) | Protocolo envelope + tolerancia a items desconocidos, probado con los SDKs reales |
| [013](../adr/0013-compatibilidad-con-sentry-cli-a-partir-de-trafico-grabado.md) | `sentry-cli`: se **graba** el tráfico de la herramienta real y se implementa lo grabado |
| [003](../adr/0003-algoritmo-de-grouping-versionado.md) | El algoritmo de agrupación se versiona, porque cambiarlo parte la historia de todo el mundo |

## 3. Por qué las consultas son baratas

La pregunta que decide si esto cabe en una máquina pequeña no es cómo se
escribe un evento, sino qué cuesta leerlo.

| ADR | Decisión |
|---|---|
| [010](../adr/0010-agregados-horarios-en-la-transaccion-de-ingesta.md) | Buckets por hora escritos **en la misma transacción** que el evento — y con su propia retención, para que el dashboard sobreviva al borrado de payloads |
| [011](../adr/0011-busqueda-de-texto-con-fts5-sobre-issues.md) | Búsqueda FTS5 sobre issues, jamás sobre eventos |
| [007](../adr/0007-percentiles-por-sketch.md) | Nunca se persiste un percentil: no son componibles |
| [020](../adr/0020-sketch-de-latencias-fusionable.md) | El histograma logarítmico que lo hace posible, con **fusión exacta** |
| [008](../adr/0008-ventana-de-sessions-en-memoria.md) | Jamás una fila por session: cuatro contadores por (release, environment, hora) |
| [033](../adr/0033-timestamps-de-ancho-fijo-en-sqlite.md) | Timestamps de ancho fijo, para que comparar cadenas sea comparar instantes |

## 4. Lo que se puede vigilar

Las features, cada una con la decisión que la define.

| ADR | Qué |
|---|---|
| [012](../adr/0012-releases-orden-y-resolucion-en-proxima-release.md) | Releases, orden entre ellas, y "resolver en la próxima release" — con la regla de reapertura que evita el falso positivo del pod sin redesplegar |
| [032](../adr/0032-la-regla-de-reapertura-en-la-transaccion-de-ingesta.md) | Dónde se aplica esa regla, y por qué ahí |
| [018](../adr/0018-source-maps-debug-ids-primero.md) | Source maps: debug ids primero, release+URL después; resueltos **al ingerir**, no al leer |
| [038](../adr/0038-el-presupuesto-de-artefactos-se-cobra-donde-se-conoce-el-proyecto.md) | Dónde se cobra el presupuesto de artefactos subidos |
| [019](../adr/0019-suspect-commits-por-interseccion-de-rutas.md) | Qué cambio trajo un error, sin hablar con GitHub |
| [021](../adr/0021-tracing-sampling-determinista-y-downsampling.md) | Tracing: sampling por hash del `trace_id`, porque un waterfall se guarda entero o no se guarda |
| [016](../adr/0016-crons-compatibles-y-ping-curl-able.md) | Vigilar algo que **no** llega: crons con check-in y ping curl-able; uptime con guardia SSRF |
| [036](../adr/0036-un-cron-que-falla-tambien-avisa.md) | Un cron que corre y falla también avisa |
| [037](../adr/0037-un-monitor-dos-familias-una-identidad.md) | Un cron y un uptime son el mismo concepto, y sus ids chocan |
| [017](../adr/0017-status-page-publica-renderizada-en-servidor.md) | La única pantalla que lee alguien de fuera, renderizada en el servidor |
| [015](../adr/0015-notificaciones-por-outbox-persistente.md) | Alertas por outbox persistente: un reinicio no pierde ni duplica |
| [035](../adr/0035-frontera-entre-el-digest-y-los-canales-de-alerta.md) | La frontera entre el digest, `doctor` y los canales |
| [031](../adr/0031-retencion-de-cero-dias-significa-no-conservar-nada.md) | Una retención de cero días significa cero, no "casi cero" |

## 5. Que un agente lo opere

El argumento del producto, escrito como decisión.

| ADR | Decisión |
|---|---|
| [022](../adr/0022-mcp-como-cuarto-cliente.md) | MCP como cuarto cliente: dos transportes, **una** tabla, y el issue bundle como documento y no como registro |
| [040](../adr/0040-la-documentacion-se-ejecuta.md) | La documentación de usuario se ejecuta en un gate, y el nombre del producto es un marcador hasta que exista |

## 6. Que sobreviva a internet

El endpoint de ingesta es público por diseño y autenticado por una credencial
que viaja en bundles de navegador. Todo lo que llega ahí lo eligió otro.

| ADR | Decisión |
|---|---|
| [023](../adr/0023-identidad-del-cliente-y-limites-por-ip.md) | De quién es una petición, y los techos por dirección |
| [039](../adr/0039-presupuesto-de-memoria-compartido-en-la-ingesta.md) | Un presupuesto de memoria **compartido** por todo lo que la ingesta descomprime a la vez — la pregunta del atacante no es cuánto cuesta una, sino cuánto cuestan mil |
| [030](../adr/0030-cors-abierto-solo-en-ingesta.md) | CORS abierto en ingesta, y en ningún otro sitio |
| [034](../adr/0034-escuchar-por-familia-de-direcciones.md) | El servidor escucha por familia de direcciones |

## 7. Dónde está la frontera

| ADR | Decisión |
|---|---|
| [004](../adr/0004-frontera-del-motor-de-eventos.md) | `internal/engine` no importa nada del repo, porque algún día es una librería Apache-2.0 aparte |
| [014](../adr/0014-scheduler-con-arranque-condicional.md) | Un solo scheduler, y un job que no se registra **no existe** |

Las siete fronteras y quién las verifica están en
[ARCHITECTURE.md](../../ARCHITECTURE.md#the-two-boundaries-that-are-never-crossed).
No son convención: `internal/arch` las comprueba en cada `go test`, y
`depguard` otra vez en el lint. Que estén duplicadas es intencional — el test
corre aunque no tengas el linter instalado.

---

## Cómo se escribe una decisión aquí

Una por fichero, numerada, **inmutable**. Si una decisión cambia, se escribe un
ADR nuevo que supersede al anterior en vez de editarlo: el valor está en poder
leer por qué algo se decidió *entonces*, con la información de entonces.

Formato: Contexto / Decisión / Consecuencias. El contexto incluye lo que se
sabía y lo que no; las consecuencias incluyen lo que se pierde, no sólo lo que
se gana.

Las marcadas **Propuesta — pendiente de ratificación** se tomaron sobre la
marcha, ante algo que el diseño inicial no anticipaba. Siguen vigentes en el
código; lo que falta es la revisión.
