# 035 — La frontera entre el digest, `doctor` y los canales de alerta

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

Digest y canales de alerta se construyeron en paralelo, en ramas separadas.
La rama de canales construye el subsistema de notificaciones completo —canales, reglas, silencios, outbox, cifrado en reposo,
los cinco adapters de entrega (ADR 015)— y la rama del digest construye dos
cosas que
**dependen de él sin ser él**: el digest semanal, que necesita saber a quién
enviarlo y por dónde, y los chequeos de canal de `doctor`, que necesitan saber
dónde entrega cada canal y si la clave de secretos lo abre.

El reparto de ficheros estaba dado: `alert_channels`, `alert_rules`,
`alert_state`, `notifications` y `internal/adapters/notify/` son de la rama de
canales en exclusiva. Lo que no estaba dado es **por dónde se hablan las dos mitades**, y
esa pregunta no se puede posponer: sin respuesta, el digest o bien no existe
hasta que la otra rama mergee —y entonces el digest entrega una plantilla sin nada
que la ejecute— o bien se inventa su propia entrega en paralelo, que es la
peor de las dos opciones porque produce dos caminos de envío que hay que
volver a fundir después.

Hay además una tentación concreta que conviene nombrar para descartarla: que
`doctor` compruebe un canal **enviándole un mensaje de prueba**. Es la
implementación de cinco minutos, y es la que garantiza que nadie ejecute el
chequeo: un comando de diagnóstico que escribe "test" en el canal del equipo se
corre una vez.

## Decisión

**Un puerto de tres métodos, `ports.AlertChannels`, y una sonda genérica de
conectividad que no pertenece al subsistema de notificaciones.**

- **`ports.AlertChannels`** declara lo mínimo que el digest y `doctor`
  necesitan: `Channels(ctx)` y `EnqueueDigest(ctx, channelID, subject, body)`.
  Lo que cruza la frontera es una lista, un destino al que conectarse y una
  forma de entregar un mensaje ya escrito. Lo que **no** cruza es ninguna
  credencial: `ports.AlertChannel` lleva `Type`, `Name`, `Digest`, `Endpoint`
  y `SecretError`, y ni un token de bot ni un secreto de webhook ni una
  contraseña de SMTP. El cifrado en reposo del ADR 015 no serviría de nada si
  los secretos viajasen en claro por tres paquetes.
- **El descifrado lo hace quien tiene la clave**, es decir el repositorio de
  canales. Su resultado se expresa en dos campos: si la clave abrió el
  registro, `Endpoint` dice a dónde entrega; si no, `SecretError` dice por qué
  no. Eso **es** la comprobación de que "la clave descifra lo que dice
  descifrar" — no hace falta un método aparte, y uno aparte se podría olvidar
  de llamar.
- **La conectividad la comprueba `internal/adapters/channelprobe`**, que no es
  parte de `notify/`. Conectarse a un host y colgar es la misma operación para
  los cinco tipos de canal, no depende de ningún protocolo de mensajería y no
  hay nada en ella que un ensamblado quiera elegir. Resuelve el nombre, abre la
  conexión, completa el handshake TLS si el endpoint es TLS, y cierra.
- **La sonda no envía nada. Nunca.** Ni un mensaje, ni una petición HTTP, ni
  una autenticación. Lo que se puede afirmar tras un probe es exactamente esto:
  el nombre resuelve, el puerto acepta, y el certificado es válido para ese
  nombre. Lo que **no** se puede afirmar es que la credencial siga siendo
  buena, y `doctor` lo dice con esas palabras en vez de dejarlo implícito. Un
  chequeo que se atribuye una verificación que no hizo es peor que uno ausente,
  porque el segundo al menos no tranquiliza a nadie.
- **Sin implementación del puerto, el digest no existe**: `Wants` responde que
  no, el job no se registra, no aparece en `/system/jobs` y `doctor` informa de
  que esta instalación no tiene subsistema de notificaciones. Es la aplicación
  literal del ADR 014 al primer job realmente condicional del producto.
  `POST /digest/preview` **sí** funciona sin canales, porque previsualizar es
  lo que alguien hace *antes* de configurar a dónde enviarlo.
- **`internal/digest` es un paquete puro** (stdlib, sin imports del repo),
  verificado por `internal/arch` y por `depguard` como las otras cuatro
  fronteras. La razón es distinta a la de `envelope` o `clientip`: aquí no hay
  input hostil, hay **fixtures dorados**, y un fixture dorado sólo significa
  algo mientras la salida sea función de la entrada y de nada más. Un render
  que pudiera leer un reloj tendría fixtures que hay que regenerar los martes.

## Consecuencias

- **Conectar las dos mitades es una línea de cableado**, no una integración:
  `wiring.Options.Channels` pasa a apuntar al repositorio de canales y todo lo demás —el job,
  el endpoint, la CLI, `doctor`— ya está escrito y probado contra un doble.
- **La rama de canales gana un requisito que no tenía**: su repositorio de canales
  debe satisfacer `ports.AlertChannels`, lo que significa exponer un `Endpoint`
  derivado de la configuración descifrada y una `EnqueueDigest` que inserte en
  `notifications` como cualquier otra entrega. Ninguna de las dos es trabajo
  nuevo; son la superficie de lo que ya construye.
- **La re-evaluación del ADR 014 queda pendiente de un cable**: el job del
  digest se decide al arrancar y cada vez que `Scheduler.Reevaluate` se llama,
  y hoy sólo lo llama la escritura de configuración de proyecto. Cuando exista
  la escritura de canales, tiene que llamarlo también — envuelta en el mismo
  truco de `wiring`, para que no pueda construirse un ensamblado que se olvide.
  Hasta entonces, activar el digest en un canal surte efecto en el siguiente
  arranque en vez de en el acto.
- **La sonda es un vector de salida deliberado**: `GET /system/channels` hace
  que el servidor abra conexiones a los hosts que sus propios canales nombran.
  Está autenticado con `projects:read` y sólo alcanza destinos que un
  administrador ya configuró, así que no amplía lo que ese administrador podía
  hacer. Cuando uptime traiga el guardia SSRF para sus monitores, la
  pregunta de si esta sonda debe pasar por él **se responde entonces**, con el
  guardia ya escrito: hoy inventarlo aquí sería decidirlo dos veces.
- **Lo que se pierde**: `doctor` no puede decir "el token es válido". Es la
  mitad que un probe no alcanza, y la única forma de cubrirla es enviar algo.
  Queda para el botón "Probar" de canales (`POST /channels/{id}/test`), que es una
  acción explícita de alguien que sabe que va a aparecer un mensaje — la
  distinción que hace que un diagnóstico se ejecute y el otro no.

## Cableado (2026-08-29)

La frontera está conectada, y esta sección la cierra en vez de dejarla
colgando. Lo que se hizo y lo que se aprendió al hacerlo:

- **`sqlite.AlertRepository` implementa `ports.AlertChannels`**, y es la única
  implementación. Por eso `wiring.Options.Channels` **desaparece**: existía
  para que dos ramas se construyeran a la vez, y mantenerlo hoy sólo sería una
  forma de ensamblar un servidor cuyo digest no tiene a dónde ir. `wiring`
  conecta el repositorio directamente, que es lo que este paquete existe para
  garantizar — un ensamblado, no dos.
- **La consecuencia "pendiente de un cable" queda cerrada.** El
  `Scheduler.Reevaluate` que trae al notificador con el primer canal trae
  también al digest con el primer canal que marca `digest`, porque
  `usecase.Alerts` ya recibía ese callback como argumento posicional. Activar
  el digest surte efecto en el acto, no en el siguiente arranque, y hay un test
  del cable concreto en `internal/wiring/wiring_test.go` — que además salda la
  deuda del backlog "internal/wiring sigue sin tests propios".
- **`Endpoint` se precisa: es un destino al que conectarse, no la URL de
  entrega.** Al cablearlo apareció una consecuencia que la decisión original no
  nombraba. `GET /system/channels` está guardado por `projects:read`, un scope
  **más débil** que el `alerts:read` que guarda el listado de canales; y para
  Telegram, Slack y Discord la URL de entrega **es** la credencial (el token va
  en el path; el path de un incoming webhook es toda su autenticación). Devolver
  la URL entera habría entregado por la puerta de atrás lo que
  `AlertChannel.Redacted` tapa por la de delante. Así que `Endpoint` conserva
  esquema, host y puerto para esos tres tipos, la URL entera para el webhook
  firmado —que se autentica por HMAC, no por su path— y `host:port` para SMTP.
  Eso es exactamente lo que `channelprobe` necesita y ni un byte más.
- **Un canal que no descifra no rompe el listado.** `Channels` reporta el fallo
  por fila en `SecretError` en vez de devolver error: la pregunta que `doctor`
  hace es *cuál* de los canales dejó de abrirse, y fallar la llamada entera
  contestaría "el listado está roto" en una instalación cuyos otros canales
  están bien.
- **El digest entra en el outbox como cualquier otra entrega.**
  `EnqueueDigest` escribe una fila `notifications` con `rule_id = 0` — ninguna
  regla la produjo — y un payload cuyo `event` es `digest`, una `TriggerKind`
  por tipo y deliberadamente **no** por pertenencia: no está en
  `AllTriggerKinds`, así que ninguna regla puede escribirse contra ella.
- **Lo que ya no es cierto del texto de arriba**: la frase "sin implementación
  del puerto, el digest no existe" describía una instalación que hoy no puede
  construirse. `usecase.ChannelHealth` sigue tolerando un `nil` porque sus
  tests lo usan, pero ningún ensamblado del producto lo produce, y `doctor`
  pasó de decir "este build no tiene subsistema de notificaciones" a decir
  "ninguno configurado", que es la verdad de una instalación que no ha querido
  alertas.
