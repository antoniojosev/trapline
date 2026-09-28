# 015 — Notificaciones por outbox persistente, con silencio y secretos en disco

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Hasta aquí el producto **ve** los errores. Alguien tiene que estar mirando el
panel para enterarse, y el objetivo de las alertas dice otra cosa: romper una
aplicación y recibir el aviso, con enlace al issue, en menos de un minuto, sin
tocar nada.

La parte difícil no es mandar un mensaje. Son las tres formas en que esto falla
en producción, y las tres tienen la misma raíz —**estado que sólo existe en
memoria**:

1. **El servidor se reinicia justo cuando hace falta.** Si la notificación
   vive en una cola en memoria mientras se intenta entregarla, un reinicio a
   mitad la pierde. Y el reinicio no es un evento raro: es lo primero que hace
   un operador cuando algo va mal, así que el momento en que este subsistema se
   queda mudo es exactamente el momento para el que existe.
2. **Lo mismo se rompe cuatrocientas veces.** Un deploy malo genera ráfagas. Sin
   una ventana de silencio, el canal se vuelve inutilizable en treinta segundos
   y alguien lo apaga. Si esa ventana vive en memoria, el reinicio del punto
   anterior la borra —y el reinicio ocurre *durante* la ráfaga, que es
   justamente cuando el silencio importa.
3. **El otro extremo está caído.** Slack tiene un incidente, el relé SMTP
   rechaza, el webhook del cliente devuelve 502. Un intento único y perdido
   convierte "no me llegó la alerta" en algo indistinguible de "no pasó nada".

Hay además un problema que no es de disponibilidad sino de superficie: un canal
guarda **credenciales de sistemas ajenos**. Un bot token de Telegram, la URL de
un incoming webhook (que *es* la autenticación), una contraseña SMTP. En una
columna en claro, el radio de impacto de un fichero `.db` filtrado deja de ser
"los reportes de error" y pasa a ser "el workspace de Slack".

## Decisión

### Outbox persistente: la fila se escribe en la transacción que la produjo

El evento de dominio que dispara una alerta —issue nuevo, regresión— **inserta
las filas de `notifications` dentro de la misma transacción que lo produce**, en
`IssueRepository.RecordEvent`. Un job `notifier` del scheduler las envía
después.

No hay ventana entre "el issue existe" y "hay algo que va a avisar de él". Esa
ventana es donde cae el reinicio, y cerrarla es la única razón por la que dos
repositorios colaboran directamente en `internal/wiring` en vez de hacerlo a
través de un caso de uso: un caso de uso serían dos transacciones, y dos
transacciones son otra vez la ventana.

La emisión **no puede tumbar la ingesta**. Va dentro de un `SAVEPOINT`: si algo
falla al encolar, se deshace sólo esa parte y el evento se guarda igual. Perder
un reporte de error para proteger una notificación es exactamente al revés en un
producto cuyo trabajo es no perder reportes de error.

Los otros dos disparadores no ocurren dentro de esa transacción y por eso abren
la suya: `issue_spike` se evalúa **después** de `RecordEvent` (necesita el
bucket de la hora en curso, que esa transacción acaba de escribir) y
`error_rate` se evalúa **antes** del limitador. En ambos casos la escritura
sigue siendo transaccional: nada de lo que decide notificar vive en memoria.

### Entrega: reintentos con backoff, y un log que es evidencia

Una fila por **canal**, no por regla: la entrega tiene éxito o falla por
destino, y un incidente de Slack no puede retener el correo.

- Backoff exponencial desde 30 s con tope de 1 h; 10 intentos y la fila queda
  `dead`. Diez intentos en ese calendario cubren unas ocho horas, que es una
  caída que alguien se duerme encima.
- **El *lease* es la propia columna `next_attempt_at`.** Un pase que reclama una
  fila la empuja al futuro con el mismo backoff que usaría un fallo. Así dos
  pases no pueden mandar lo mismo, y no hace falta un quinto estado
  "enviando" —un estado que, viviendo en una base de datos, necesitaría su
  propio recolector para las filas que dejó ahí un crash.
- **La entrega es *at-least-once*, y se dice.** Un proceso que muere entre un
  POST correcto y el `UPDATE ... status='sent'` produce una segunda entrega. Es
  irreducible sin un commit distribuido con el otro extremo, así que en lugar de
  fingir lo contrario se emite `X-Errtrack-Delivery`, estable entre reintentos,
  para que el receptor deduplique.
- El intervalo del job es **1 s**, y eso es deliberado: ese tick *es* la latencia
  de la alerta. Cuesta una consulta indexada por segundo contra una tabla cuyo
  estado estable son filas ya enviadas —y sólo en una instalación que configuró
  un canal, porque si no el job no existe.
- El log se poda: las filas `sent` mayores de 30 días se borran (barrido horario
  dentro del propio notifier). Las `dead` **no**: son la respuesta a "¿por qué no
  me llegó?", que es la pregunta cara.

### Silencio persistido, por sujeto

`alert_state(rule_id, subject_key, last_fired_at)`. Si la ventana no expiró, la
fila **no se inserta** —no se inserta y luego se filtra: no llega a existir.

La clave es el **sujeto** (`issue:<id>`, `project:<id>`, y `monitor:<id>` para monitores),
no la regla. Un deploy que rompe dos endpoints debe producir dos avisos y luego
callarse sobre ambos; un silencio por regla dejaría que el primer issue tapara al
segundo, que es el comportamiento que hace que la gente apague las alertas.

### Secretos cifrados en reposo, con la clave fuera del `.db`

`config_enc` es AES-256-GCM. La clave vive en `<db>.key`, modo 0600, generada
**al primer uso** —no al arrancar: una instalación que nunca configura un canal
no tiene por qué tener un fichero de clave del que nadie le ha hablado. Es la
promesa del ADR 005 aplicada a un fichero. Se puede fijar con
`-secret-key-file` / `ERRTRACK_SECRET_KEY_FILE`.

Y de ahí sale la consecuencia que hay que gritar: **`backup` copia el `.db` y no
copia el `.key`**. El día de la restauración los canales aparecen listados y no
entrega ninguno, sin nada en los logs que apunte al fichero que falta. Por eso
`backup` lo dice en cada ejecución, nombrando la ruta, en texto y en `--json`; y
`doctor` comprueba permisos y que la clave **descifra un canal real** —sólo si
hay canales, porque si no, no hay subsistema del que informar.

`doctor` **nunca crea** la clave: comprueba antes de abrir. Un diagnóstico que
acuñara una clave nueva convertiría "te falta el fichero" en "perdiste todos tus
canales", en silencio y para siempre.

### Reglas sin DSL: cuatro disparadores con parámetros

`new_issue`, `regression`, `issue_spike{window_s, min_count, factor}`,
`error_rate{window_s, min_events_per_min}`. Un lenguaje de consulta sería un
segundo producto que diseñar, documentar y mantener compatible; estas cuatro son
las preguntas que alguien le hace de verdad a un error tracker: ¿es nuevo?,
¿volvió?, ¿va a peor?, ¿está todo ardiendo? Los monitores añaden sus disparadores
a la lista. Un quinto tipo distinto sería el momento de parar y
diseñar el lenguaje en serio.

**`error_rate` se cuenta ANTES del limitador.** Es la línea que ya estaba escrita
en el ADR 005 y es la que se descubre en producción si se hace al revés: si se
contara después, una ráfaga lo bastante grande como para ser rechazada
silenciaría la alerta sobre sí misma, exactamente cuando la alerta es el punto.
El contador es una ventana deslizante de 5 minutos en cubos de un minuto, **en
memoria**, y ahí sí es lo correcto: lo que se cuenta incluye los eventos que el
limitador está a punto de tirar, y una escritura a disco por cada uno haría que
rechazar una ráfaga costara más que aceptarla. Está acotado (512 proyectos, con
desalojo del menos reciente) porque un mapa sin techo en un producto que promete
30 MB es una promesa con un agujero. Consecuencia declarada: `window_s` de
`error_rate` no puede exceder 5 minutos —una regla que pidiera una media de seis
horas le estaría preguntando algo a un contador de cinco minutos, y responderle
aproximadamente sería peor que rechazarla, porque el número parecería correcto.

**`issue_spike` se evalúa tras `RecordEvent`**, sobre `issue_hourly` (ADR 010)
más la hora en curso, que esa misma transacción acaba de escribir. Preguntarlo
antes compararía una hora a la que le falta su evento más nuevo contra una línea
base a la que no le falta nada, y estaría mal en la dirección de no disparar
nunca. `window_s` se redondea **hacia arriba** a horas enteras, porque los
agregados son horarios, y la API lo dice en vez de fingir una resolución que no
tiene. La evaluación se estrangula a una por issue cada 30 s: un spike es una
afirmación sobre una hora, y preguntarlo por evento sería una consulta por evento
ingerido.

### Webhook firmado

`X-Errtrack-Signature: t=<unix>,v1=<hex hmac-sha256(secret, t + "." + body))>`,
más `X-Errtrack-Event` y `X-Errtrack-Delivery`. El timestamp va **dentro** del
material firmado: una firma sólo sobre el cuerpo es replicable para siempre, y
una sobre el cuerpo más un timestamp sin firmar es replicable por cualquiera que
sepa editar una cabecera. El prefijo `v1=` es lo que permite añadir un segundo
esquema sin romper a quien ya escribió su receptor.

Se firma **exactamente el cuerpo que se envía**, byte a byte. Un receptor que
re-serializara el payload y verificara eso fallaría ante cualquier diferencia de
orden de claves, que es la forma clásica en que una firma de webhook acaba
documentada como "no funciona, desactiva la comprobación".

El secreto mínimo es de 16 caracteres y **no es opcional**: un webhook sin firmar
es un endpoint al que puede postear cualquiera que aprenda la URL.

### `base_url` por canal, y por qué existe

Telegram, Slack y Discord aceptan un `base_url` que sustituye la raíz de su API
—no la ruta. Existe **para los tests**: el gate apunta los tres al mismo receptor
local, que imita sus rutas reales (`/bot<token>/sendMessage`, `/services/...`,
`/api/webhooks/...`). Se declara aquí porque un ajuste de producto que en
realidad es una costura de pruebas, sin decirlo, es deuda disfrazada de feature.
Una instalación real lo deja vacío. Sustituye el origen y conserva la ruta: un
test que posteara a `/` demostraría que el adapter sabe hacer un POST, cosa que
nadie dudaba.

### Scopes propios

`alerts:read` y `alerts:write` —la primera área del producto con par propio. Los
scopes aquí son gruesos a propósito (`internal/domain/token.go`), pero un canal
guarda una credencial de un sistema ajeno, así que "puede leer la lista de
issues" y "puede leer a dónde manda sus avisos esta instalación" son permisos de
verdad distintos. Un token creado antes de este build no los tiene, y eso es
correcto, no una regresión: el mensaje de error nombra el scope que falta.

## Consecuencias

- `RecordEvent` tiene un colaborador más y una responsabilidad más. El coste por
  evento en una instalación **sin** reglas es una lectura de un slice cacheado;
  el bench sigue por encima del gate.
- La caché de reglas se invalida en las escrituras del propio repositorio, que
  son el único camino para cambiarlas —el mismo truco que el limitador, por la
  misma lección: un callback que hay que acordarse de cablear funciona en
  producción y silenciosamente no en un test que ensambla distinto (ADR 005).
- El job `notifier` sólo existe si hay ≥1 canal, y aparece o desaparece **sin
  reiniciar** porque la escritura de canales avisa al scheduler (ADR 014). El
  gate lo comprueba en los dos sentidos. Esto cierra lo que el ADR 014 dejó
  pendiente: qué explicación hace falta cuando un subsistema no aparece. La
  respuesta resultó ser ninguna —el gate afirma la ausencia, y el operador que
  añade un canal ve el job aparecer mientras mira.
- **Un backup ya no es autosuficiente.** Es el precio de no guardar la clave
  dentro de lo que cifra, y se paga con un aviso en cada `backup` y una
  comprobación en `doctor`.
- La entrega es *at-least-once*. Quien reciba webhooks debe deduplicar por
  `X-Errtrack-Delivery`; está documentado en `docs/alerts/webhooks.md` con el
  código que el propio gate ejecuta.
- **`notifications` crece.** Se poda a 30 días para las entregadas, y esa poda la
  hace el notifier y no la retención: la retención no debe conocer una tabla de
  un subsistema que nadie encendió.
- El tick de 1 s es visible en `GET /system/jobs`. Si alguna vez molesta, la
  alternativa es un aviso en memoria desde la ingesta con el tick como red de
  seguridad —posible sin cambiar nada de lo escrito aquí, porque el aviso sería
  una optimización y no una fuente de verdad.
- Sin dependencias nuevas: `net/http` y `net/smtp` de la stdlib. Cinco SDKs
  oficiales serían cinco árboles de dependencias, cinco cadencias de release y
  cinco superficies de vulnerabilidad para lo que en cuatro casos es un POST con
  un cuerpo JSON y en el quinto una conversación SMTP que no ha cambiado desde
  antes de que existiera Go.
