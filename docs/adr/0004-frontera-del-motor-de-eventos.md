# 004 — Frontera del motor de eventos

- **Fecha**: 2026-08-22
- **Estado**: Aceptada (2026-08-24)

## Contexto

Todo lo que este producto almacena es un evento con dimensiones: errores,
transactions, sessions, resultados de uptime y check-ins de crons. Un motor genérico
de eventos (ingesta con batching → storage → agregación por ventanas → retención) es
la primitiva de la que cada feature es una vista.

El plan original preveía **extraer ese motor de Traccia** (un producto de analytics
propio, ya público) como librería Go compartida, y que Traccia migrara a
consumirla. La revisión del código de Traccia (2026-08-22) muestra que ese motor
**no existe** ahí: lo que hay es un `EventRepository` específico de Postgres con
columnas de analytics en el SQL (`path`, `referrer`, `visitor_id`, `device_type`),
un `INSERT` sincrónico por evento, y agregación calculada en vivo con
`date_trunc` sobre la tabla raw. Sin batching, sin backpressure, sin tablas
agregadas, sin downsampling, sin retención.

O sea que "extraer" sería en realidad **escribir el motor nuevo y retrofitear
Traccia**: construir una librería con dos adapters de storage antes de que ningún
consumidor la haya ejercitado. Generalidad especulativa, y además contradice el
principio propio de extraer al segundo consumidor y no antes.

## Decisión

**El motor se construye dentro de este repo, en `internal/engine`, con una frontera
de puertos disciplinada. La extracción a librería standalone se aplaza hasta que
exista un segundo consumidor real que la necesite.**

`internal/` es deliberado: Go impide que nadie importe el paquete desde fuera, así
que la frontera no puede erosionarse por accidente antes de estar lista.

**La frontera** (lo que sería la API de la librería el día de la extracción):

- **Dentro**: evento genérico (timestamp, tipo, payload, dimensiones); pipeline de
  ingesta con batching y backpressure; agregación por ventanas con downsampling;
  sketches de percentiles fusionables (ADR 007); retención por categoría.
- **Fuera**: todo lo específico de este dominio — parsing de envelopes, grouping,
  symbolication, reglas de alerta. Vive en el repo, nunca en el motor.

## Consecuencias

- **Aceptada por el mantenedor el 2026-08-24**, con la evidencia del código de
  Traccia a la vista. El plan de fases queda revisado: F0a deja de ser "extraer
  el motor" y pasa a ser "construirlo aquí con la frontera puesta"; la
  extracción a librería se replanteará cuando Traccia tenga una razón propia
  para migrar.
- El producto arranca de inmediato, sin 3 semanas de trabajo especulativo previo.
- **Desaparece el riesgo de romper o retrasar Traccia**, que el plan calificaba como
  probabilidad media, y a cambio de nada: Traccia no gana nada migrando hoy (no
  necesita batching ni percentiles) y es un proyecto vivo ya tagueado.
- El motor se curte con un consumidor real antes de convertirse en API pública. Las
  APIs de librería que nadie ha usado se diseñan mal.
- Se aplaza la narrativa de "una librería para toda la casa". El tercer consumidor
  previsto está a meses, así que el coste temporal es nulo.
- **Disciplina obligatoria**: `internal/engine` no importa nada del resto del repo.
  El gate de arquitectura de CI lo verifica. Si esa regla se relaja, la extracción
  futura deja de ser barata y esta decisión pierde su sentido.
- Cuando llegue la extracción: la librería nace **Apache-2.0**, no AGPL — es código
  para reutilizar, y AGPL en una librería infectaría a todo lo que la importe.
