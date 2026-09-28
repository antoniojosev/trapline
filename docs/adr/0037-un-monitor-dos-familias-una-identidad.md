# 037 — Un monitor, dos familias, una identidad

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

El ADR 016 describe dos subsistemas bajo una misma cabecera: los **cron
monitors**, que vigilan algo que debería reportarse, y los **uptime monitors**,
que vigilan preguntando. Crons y uptime se implementaron **en paralelo**, en
ramas separadas, sobre
las mismas estructuras del dominio y sin verse la una a la otra.

Al fusionarlos, cinco ficheros del dominio traían dos versiones de la misma
cosa: dos listas de triggers, dos `switch` de validación, dos ramas del
renderizador de cabeceras, dos `Origin.MonitorURL`, dos comentarios sobre por
qué `monitors:read|write` existe, y dos despachadores `monitors` en la CLI —el
segundo de los cuales habría **tapado** al primero sin que Go dijera nada, por
estar en ficheros distintos del mismo paquete.

Resolverlos «conservando ambas adiciones» habría producido dos jerarquías
paralelas para un concepto que el usuario ve como uno: una pantalla, un scope,
una palabra en la CLI, una página de estado. Pero al ponerlos uno al lado del
otro apareció además un fallo real que ninguna de las dos ramas podía ver por
su cuenta:

**Los ids vienen de dos tablas.** `cron_monitors.id` y `uptime_monitors.id` son
secuencias independientes, así que el monitor cron 7 y el monitor uptime 7
existen a la vez y no tienen nada que ver. Las dos ramas escribían la clave de
sujeto como `monitor:7` y el enlace del panel como
`/projects/2/monitors/7`. Fusionadas sin más, eso significa:

1. Un uptime monitor que se cae **silencia** la alerta de un cron monitor que
   comparte id, porque la ventana de silencio del ADR 015 es por sujeto. Es
   exactamente el fallo que esa ventana existe para no cometer, entregado por
   la propia corrección.
2. El enlace de la notificación abre el monitor equivocado —o ninguno.
3. Un receptor de webhook que guarde estado por `monitor_id` mezcla los dos.

Ninguno de los tres se habría visto en los gates de cada rama: hacen falta las
dos familias, con ids que coincidan, en la misma instalación.

## Decisión

**Un monitor es un concepto con dos familias, y la familia forma parte de su
identidad en todas partes.**

- `domain.MonitorKind` (`cron` | `uptime`) en `internal/domain/monitor.go`, con
  `AllMonitorKinds()` y `Valid()`, junto a la única `MonitorSubjectKey`.
- **Clave de sujeto**: `monitor:<familia>:<id>`. Es lo que corrige el
  solapamiento de la ventana de silencio. (El ADR 036 la escribe como
  `monitor:<id>`; esta decisión la reemplaza en ese detalle, sin tocar nada de
  lo que 036 decide.)
- **URL del panel**: `Origin.MonitorURL(projectID, kind, monitorID)` →
  `/projects/{id}/monitors/{familia}/{id}`. Un solo método.
- **Payload de alerta**: `monitor_kind` junto a `monitor_id` y `monitor`. El
  campo legible (`monitor`) que introdujeron los crons se conserva y pasa a servir a
  las dos familias: el slug para un cron, el nombre para un uptime.
- **Una lista, no dos**: `AllTriggerKinds()` y `MonitorTriggerKinds()` cubren
  los seis triggers de monitor, y los `switch` de validación y de render los
  tratan juntos.
- **Un despachador**: `monitors cron …` y `monitors uptime …` cuelgan del mismo
  `runMonitors`.
- **Un `ErrInvalidMonitor`, un `ErrMonitorNotFound`, un par de scopes**, con
  las dos razones documentadas juntas.

Lo que **no** se unifica: las tablas, los repositorios, los casos de uso y las
rutas REST siguen separados. Las dos familias tienen campos, jobs y estados
distintos, y meterlas en una tabla con la mitad de las columnas nulas sería
unificar lo que de verdad difiere para poder decir que están unificadas.

## Consecuencias

- **Cambia la forma de dos cosas que ya estaban en `main`**: la clave de sujeto
  de un cron monitor y su URL de panel. Ambas son internas o no tenían consumo
  todavía —la pantalla del panel llega con este mismo cambio, y no hay instalación
  desplegada—, así que el coste es cero hoy y no lo sería nunca más.
- Una fila de `notifications.subject_key` escrita por un build anterior con
  `monitor:7` deja de silenciar a la nueva. El efecto máximo es **una** alerta
  repetida por monitor al actualizar, y ninguna alerta perdida, que es el lado
  correcto en el que equivocarse. No hay migración: la tabla se poda sola.
- `docs/alerts/webhooks.md` documenta `monitor_kind` y advierte de la colisión
  a los receptores que enruten por `monitor_id`.
- La página de estado (ADR 017) y `Monitors.tsx` pueden listar las dos familias
  en una sola pantalla sin inventarse un discriminador propio.
- Si algún día hay una tercera familia (un check de certificado, un check de
  puerto TCP), entra como un valor más de `MonitorKind` y hereda la ventana de
  silencio, el enlace, el scope y la pantalla sin tocar ninguno de los cuatro.
