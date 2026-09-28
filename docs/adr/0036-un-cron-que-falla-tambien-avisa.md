# 036 — Un cron que corre y falla también avisa

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

El ADR 015 enumera los triggers de monitores y para crons nombra tres:
`cron_missed`, `cron_timeout` y `cron_recovered`. Los tres
describen **silencio**: el trabajo no arrancó, el trabajo arrancó y no volvió,
el trabajo volvió a hablar.

Pero el conjunto de estados que el mismo documento fija tiene cinco valores, y
uno de ellos no es silencio: `error`. Llega cuando un check-in reporta
`status: "error"` —el decorador del SDK lo emite cuando la función lanza, y
`curl …/ping/<key>/fail` lo emite cuando un script decide que su trabajo salió
mal.

Con los tres triggers del plan, ese caso mueve el estado del monitor a `error`
y **no se lo dice a nadie**. Un backup que corrió, falló y lo reportó
correctamente es exactamente el escenario que esta función existe para cubrir,
y sería el único de los cinco estados que no produce mensaje. Peor: el estado
en el panel diría `error` y el operador se enteraría cuando entrara a mirar,
que es la conducta que ningún sistema de alertas puede permitirse asumir.

La alternativa dentro del vocabulario existente sería mapear un check-in
fallido a `cron_missed`. Se descartó: el mensaje diría «no hizo check-in» sobre
un trabajo que sí lo hizo, y una alerta que describe mal lo que pasó manda a
alguien a mirar el crontab cuando el problema está en el script.

## Decisión

**Se añade un cuarto trigger, `cron_failed`.** Se dispara cuando un check-in
reporta `error` y el monitor **no estaba ya** en `error`.

- No sólo en la transición desde `ok`: un trabajo que llevaba una hora
  reportándose como perdido y ahora devuelve un error ha cambiado *qué* tiene
  roto, y eso merece decirse. Lo que evita el ruido es la ventana de silencio
  del ADR 015, que ya es por sujeto (`monitor:<id>`).
- `cron_recovered` pasa a cubrir también la salida de `error`, que es lo que
  ya hacía `CronStatus.Failing()`.
- Nivel `error` en el payload, igual que `cron_missed` y `cron_timeout`;
  `cron_recovered` es el único `info`.

## Consecuencias

- `domain.AllTriggerKinds()` tiene ocho entradas en vez de siete, así que una
  regla escrita contra `cron_failed` valida y se puede crear desde REST, CLI y
  —cuando exista— la UI, que tendrá que ofrecerlo en su selector.
- Es **aditivo**: ninguna regla existente cambia de comportamiento, y una
  instalación que no cree la regla no recibe nada nuevo.
- Si el mantenedor lo rechaza, el camino de vuelta es borrar la constante, su
  rama en `ApplyCheckIn` y su caso en `Headline()`. Nada más depende de él: el
  estado `error` seguiría existiendo y seguiría siendo mudo.
- Queda pendiente lo que Sentry llama `failure_issue_threshold` y
  `recovery_threshold` —«avisa tras N fallos seguidos»—. Este build ignora
  ambos campos si llegan en `monitor_config`. Con un solo trigger por
  transición el umbral efectivo es 1, que es el que quiere quien monitoriza un
  backup nocturno; un umbral configurable es una decisión aparte y no hay
  todavía nadie que la haya pedido.
