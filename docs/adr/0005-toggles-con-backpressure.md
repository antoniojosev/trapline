# 005 — Toggles con costo cero vía backpressure del protocolo

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

El competidor es todo-o-nada: instalas la plataforma entera y pagas la RAM de
features que no usas. Que aquí los subsistemas sean opcionales no basta como
promesa; **un toggle que sigue costando recursos no es un toggle**, es una casilla
de configuración.

Hay dos tipos de subsistema, con dos problemas distintos:

- **Scheduled/pull** (uptime, crons, status page, digest): el trabajo lo inicia el
  servidor.
- **Push/ingesta** (errores, tracing, sessions): el trabajo lo inicia el SDK del
  cliente. Aunque el servidor descarte el evento, ya pagó el ancho de banda, el
  decode y el request.

## Decisión

**Todo subsistema es opt-in, y apagado su costo es cero.**

- **Scheduled**: el goroutine **no arranca**. No es un bucle que comprueba una flag
  y duerme: no existe.
- **Ingesta**: se usa el mecanismo del propio protocolo. Categoría apagada o sobre
  el límite → `429` con `Retry-After` y
  `X-Sentry-Rate-Limits: <segundos>:<categorías>:<scope>`. Los SDKs oficiales lo
  respetan y **dejan de enviar esa categoría**.

**Perfil mínimo por defecto**: una instalación nueva arranca solo con error
tracking. Cada subsistema se enciende explícitamente, global o por proyecto.

## Consecuencias

- **El costo es marginal ~cero, no literalmente cero**, y conviene decirlo así en
  público: el SDK respeta el límite durante la ventana indicada y al expirar
  reintenta. Se mitiga con duraciones largas re-enviadas en cada respuesta, pero un
  ping periódico existe. Sobrevender esto como "cero absoluto" es regalarle a
  cualquiera la refutación del claim central del producto.
- La configuración de toggles, sampling y retención es **una columna por proyecto**,
  no un ajuste global. Dos proyectos en la misma instalación pueden tener perfiles
  distintos.
- La RAM idle en perfil mínimo se publica con metodología reproducible y hay
  presupuesto en CI que avisa si crece. Un número publicado sin gate se degrada solo.
- **Y el gate ya sirvió de algo.** La primera medición real dio 142 MB, no <30:
  un solo login con Argon2id a 64 MiB de coste de memoria llevaba la RSS de
  9,6 MB a 142 MB para siempre, porque Go no devuelve el heap liberado al
  sistema operativo. Se corrigió bajando el coste al recomendado por OWASP
  (19 MiB) y devolviendo el working set al SO tras la ráfaga, coalescido para
  que un flood de logins no fuerce un scavenge por petición. Reposo: ~10 MB;
  pico transitorio durante autenticación: ~50 MB. **Las dos cifras se publican;
  medir solo la primera sería técnicamente cierto y materialmente engañoso.**
- La señal de spike para alerting se evalúa **antes** del corte de rate limit: si no,
  un spike silenciaría su propia alerta justo cuando importa.
