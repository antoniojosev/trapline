# Architecture Decision Records

Una decisión por fichero, numerada e inmutable: si una decisión cambia, se escribe
un ADR nuevo que **supersede** al anterior en vez de editarlo. El valor está en
poder leer por qué algo se decidió *entonces*, con la información de entonces.

| # | Decisión | Estado |
|---|---|---|
| [001](0001-sqlite-como-unico-estado.md) | SQLite como único estado (DuckDB diferido) | Aceptada |
| [002](0002-compatibilidad-envelope-sentry.md) | Compatibilidad con el protocolo envelope + tolerancia a items desconocidos | Aceptada |
| [003](0003-algoritmo-de-grouping-versionado.md) | El algoritmo de grouping se versiona | Aceptada |
| [004](0004-frontera-del-motor-de-eventos.md) | Frontera del motor de eventos | Aceptada |
| [005](0005-toggles-con-backpressure.md) | Toggles con costo cero vía backpressure del protocolo | Aceptada |
| [006](0006-una-api-cuatro-clientes.md) | Una sola API, cuatro clientes | Aceptada |
| [007](0007-percentiles-por-sketch.md) | Percentiles por sketch fusionable, nunca pre-calculados | Aceptada |
| [008](0008-ventana-de-sessions-en-memoria.md) | Agregación de sessions con ventana en memoria | Aceptada |
| [009](0009-sqlite-puro-go-sin-cgo.md) | Driver SQLite puro Go (sin CGO) | Aceptada |
| [010](0010-agregados-horarios-en-la-transaccion-de-ingesta.md) | Agregados horarios escritos en la transacción de ingesta | Aceptada |
| [011](0011-busqueda-de-texto-con-fts5-sobre-issues.md) | Búsqueda de texto con FTS5 sobre issues, nunca sobre eventos | Aceptada |
| [012](0012-releases-orden-y-resolucion-en-proxima-release.md) | Releases, orden entre releases y resolución "en la próxima release" | Aceptada |
| [013](0013-compatibilidad-con-sentry-cli-a-partir-de-trafico-grabado.md) | Compatibilidad con `sentry-cli`: superficie `/api/0/` emulada a partir de tráfico grabado | Aceptada |
| [014](0014-scheduler-con-arranque-condicional.md) | Scheduler único con arranque condicional | Aceptada |
| [015](0015-notificaciones-por-outbox-persistente.md) | Notificaciones por outbox persistente, con silencio y secretos en disco | Aceptada |
| [016](0016-crons-compatibles-y-ping-curl-able.md) | Crons compatibles con check-ins de Sentry + ping curl-able; uptime con guardia SSRF | Aceptada |
| [017](0017-status-page-publica-renderizada-en-servidor.md) | Página de estado pública renderizada en el servidor | Aceptada |
| [023](0023-identidad-del-cliente-y-limites-por-ip.md) | Identidad del cliente y límites por IP | Aceptada |
| [030](0030-cors-abierto-solo-en-ingesta.md) | CORS abierto en ingesta, y en ningún otro sitio | Propuesta |
| [031](0031-retencion-de-cero-dias-significa-no-conservar-nada.md) | Una retención de cero días significa no conservar nada | Propuesta |
| [032](0032-la-regla-de-reapertura-en-la-transaccion-de-ingesta.md) | La regla de reapertura se aplica en la transacción de ingesta | Propuesta |
| [033](0033-timestamps-de-ancho-fijo-en-sqlite.md) | Los timestamps se guardan con ancho fijo | Aceptada |
| [034](0034-escuchar-por-familia-de-direcciones.md) | El servidor escucha por familia de direcciones | Propuesta |
| [035](0035-frontera-entre-el-digest-y-los-canales-de-alerta.md) | La frontera entre el digest, `doctor` y los canales de alerta | Propuesta |
| [036](0036-un-cron-que-falla-tambien-avisa.md) | Un cron que corre y falla también avisa | Propuesta |
| [037](0037-un-monitor-dos-familias-una-identidad.md) | Un monitor, dos familias, una identidad | Propuesta |
| [020](0020-sketch-de-latencias-fusionable.md) | Sketch de latencias: histograma logarítmico fusionable en `engine/sketch` | Aceptada |
| [021](0021-tracing-sampling-determinista-y-downsampling.md) | Tracing: sampling determinista por trace, agregados por minuto con downsampling | Aceptada |
| [018](0018-source-maps-debug-ids-primero.md) | Source maps: debug ids primero, release + URL después | Aceptada |
| [038](0038-el-presupuesto-de-artefactos-se-cobra-donde-se-conoce-el-proyecto.md) | El presupuesto de artefactos se cobra donde se conoce el proyecto | Propuesta |
| [019](0019-suspect-commits-por-interseccion-de-rutas.md) | Suspect commits por intersección de rutas | Aceptada |
| [022](0022-mcp-como-cuarto-cliente.md) | MCP como cuarto cliente: dos transportes, una tabla, y el issue bundle | Aceptada |
| [039](0039-presupuesto-de-memoria-compartido-en-la-ingesta.md) | Un presupuesto de memoria compartido para todo lo que la ingesta descomprime a la vez | Propuesta |
| [040](0040-la-documentacion-se-ejecuta.md) | La documentación de usuario se ejecuta; `{{NAME}}` hasta que haya nombre | Propuesta |
