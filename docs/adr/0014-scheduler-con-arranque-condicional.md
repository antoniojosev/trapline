# 014 — Scheduler único con arranque condicional

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El ADR 005 promete que un subsistema apagado **no cuesta**, y para los
subsistemas *scheduled* lo dice con una frase concreta: el goroutine no
arranca; no es un bucle que comprueba una bandera y duerme, es que no existe.

Hasta ahora esa promesa no tenía a quién aplicarse. El único trabajo periódico
era la retención, arrancada con un `go a.retention.Run(ctx)` en `app.go`, con su
propio `ticker`, su propio manejo de errores y su propio criterio sobre cuándo
parar. Funciona para uno. Con alertas en adelante hay varios —notificaciones,
digest semanal, monitores de cron, uptime, downsampling— y cada uno copiando ese
bucle significa cinco sitios donde equivocarse de la misma forma cinco veces, y
cinco criterios distintos sobre qué hacer cuando `Run` devuelve error.

El problema de fondo no es la duplicación, es que **el arranque condicional se
convertiría en una convención**. Una convención se cumple recordándola, y este
repo ya tiene una lección cara sobre eso: la invalidación de la caché de
configuración era un callback que había que acordarse de cablear, funcionaba en
producción y silenciosamente no en un test que ensamblaba distinto (ADR 005,
`internal/adapters/ratelimit`). Se arregló haciendo que el único camino para
escribir configuración pasara por lo que tiene que reaccionar.

Hay además un segundo agujero: un job que corre en un goroutine es invisible.
Si falla cada hora durante una semana, el servidor responde igual de sano a todo
lo demás que un operador puede preguntarle.

## Decisión

**Un paquete `internal/scheduler` con un único contrato de job, y el arranque
condicional decidido por el scheduler y no por cada job.**

- **El contrato**: `Job{ Name() string; Interval() time.Duration; Run(ctx) error }`.
  Tres métodos y ningún ciclo de vida: un job no se arranca, no se para y no se
  reconfigura a sí mismo, así que no hay estado sobre el que él y el scheduler
  puedan discrepar.
- **Un job no se registra, se *ofrece***: se registra junto con la pregunta
  «¿este subsistema tiene algo que hacer?» (`Wants func(ctx) (bool, error)`).
  Sólo los que responden que sí llegan a tener goroutine. **Un job no registrado
  no existe**: no aparece en el listado, no consume un timer, no está.
- **La pregunta se hace al arrancar y cada vez que cambia la configuración.** La
  re-evaluación la dispara la escritura de configuración, envuelta en el
  ensamblado (`internal/wiring`), con el mismo criterio que el limitador: el
  único camino para escribir configuración es el que avisa, así que no puede
  construirse un ensamblado que se olvide. Encender un subsistema surte efecto
  mientras quien lo encendió sigue mirando la pantalla; apagarlo devuelve el
  goroutine.
- **Cada job corre en su goroutine, trabaja y luego espera**, con jitter de
  ±10 % en cada espera. Trabaja antes de esperar porque un servidor que estuvo
  caído una semana vuelve con una semana de trabajo atrasado. El jitter es por
  tick y no un desfase inicial: dos jobs que coincidan no se quedan
  coincidiendo, y lo que se protege es el único escritor de SQLite que tiene
  este producto.
- **Un `Run` que devuelve error no para nada.** El scheduler lo registra, lo
  cuenta, lo publica y programa el siguiente tick. Un `Run` que hace *panic*
  se convierte en error: un error tracker que se muere porque su propia
  limpieza tocó un puntero nulo es un mal argumento a favor de sí mismo.
- **La retención se registra siempre, sin condición**, y es la única excepción
  escrita a la regla del ADR 005. La retención no es opt-in: sin ella un
  almacén de un solo fichero crece hasta llenar el disco, y lo que se promete
  es un servidor que nadie tiene que atender. La regla protege a la gente de
  pagar por funciones que no pidió; nadie pide crecimiento sin límite.
- **Observabilidad**: `GET /api/v1-beta/system/jobs` devuelve los jobs en
  marcha con `last_run`, `last_error` y `next_run`, y `errtrack doctor` los
  muestra —en texto y, con los campos intactos, en `--json` (ADR 006).
  Sólo los que están en marcha: un subsistema apagado no aparece como
  «inactivo», porque «no existe» es exactamente lo que afirma el ADR 005 y una
  fila diciendo otra cosa lo suavizaría hasta volverlo falso.
- **Sólo stdlib.** El scheduler no importa nada del repo, y los casos de uso
  satisfacen el contrato **estructuralmente**, sin importar el paquete que los
  ejecuta. `usecase.RetentionJob` no sabe que existe un scheduler.

## Consecuencias

- `Retention.Run` desaparece: la retención pasa a ser `Retention.Job()`, un
  `Sweep` y nada más. El bucle, el log de errores y la decisión de continuar
  tras un fallo son ahora del scheduler, así que ningún job futuro tiene que
  acordarse de replicarlos.
- `app.go` deja de arrancar goroutines a mano. Arranca el scheduler y lo para
  después del drenaje del servidor, para que un barrido a medio lote termine en
  vez de morir con el proceso.
- **Un error de cableado impide arrancar el servidor**: dos jobs con el mismo
  nombre, o un intervalo que no es positivo, hacen fallar `Start`. No son
  condiciones que mejoren manejándose en caliente —un intervalo cero es un bucle
  ocupado, y dos jobs con un nombre convierten el endpoint de estado en una
  mentira—, así que se rechaza el arranque en vez de degradar.
- `GET /system/jobs` se lee con `projects:read` y no con un scope propio. Los
  scopes de este producto son gruesos a propósito (`internal/domain/token.go`):
  la división que importa es lectura contra escritura, y acuñar `system:read`
  dejaría fuera a todo token creado antes de este build para un endpoint que
  sólo cuenta lo que ya está en el log. Cuando haya permisos de verdad —varios
  usuarios, varios equipos— un scope propio supersederá esta línea de la tabla
  de rutas.
- El coste de la promesa del ADR 005 pasa a ser **medible y no argumentable**:
  «cuántos goroutines de fondo tiene una instalación en perfil mínimo» se
  responde con una petición, y la respuesta es uno.
- Queda pendiente para las alertas: un subsistema apagado no dice *por qué* lo está. Un
  operador que espera ver el `notifier` y no lo ve tiene que ir a buscar si es
  que no hay canales configurados. Se resolverá cuando exista el primer job
  condicional de verdad, que es cuando se sabrá qué explicación hace falta.
