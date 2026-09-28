# 008 — Agregación de sessions con ventana en memoria

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

El *release health* (crash-free rate por release) se calcula sobre sessions. La
regla de diseño es **jamás una fila por session**: un producto de sesiones a escala
es un producto distinto, y ahí es donde el competidor gasta su infraestructura.

Pero los SDKs no envían una session cerrada: envían **updates** de la misma session
(un `init`, luego un `exit` o `crashed`). Contar sessions distintas requiere saber
que dos updates son la misma session — y eso es estado.

## Decisión

Una **ventana de agregación en memoria** que recuerda los IDs de session en vuelo
durante una ventana acotada, y solo escribe **contadores agregados** por
(release, hora): iniciadas, crasheadas, erroradas. El crash-free rate se deriva de
esos contadores.

## Consecuencias

- **Un restart pierde las sessions en vuelo de esa ventana.** Es un trade-off
  aceptado, no un descuido: la alternativa es persistir estado por session, que es
  exactamente lo que esta decisión evita. El impacto es un sesgo mínimo y transitorio
  en un ratio, no la pérdida de un error reportado. Queda documentado de cara al
  usuario en vez de ser una sorpresa.
- La ventana está acotada en memoria: una avalancha de sessions no puede crecer sin
  límite. Al llenarse se sacrifica precisión, nunca estabilidad.
- Las sessions nunca son consultables individualmente. Es una consecuencia
  deliberada: cierra la puerta a que el producto derive hacia analítica de sesiones.

## Implementación (2026-08-29)

La decisión de arriba no cambia. Lo que sigue es cómo quedó ejecutada, con los
números medidos y con las cuatro cosas que hubo que decidir para escribirla.

### La ventana

`domain.SessionWindow` (`internal/domain/release_health.go`) es una estructura
de datos pura: sin reloj, sin lock y sin almacenamiento. Cada método recibe el
instante que necesita como argumento, y quien la usa —`usecase.Health`— es
quien tiene el mutex y quien escribe. Así lo difícil (el LRU, el TTL, la fusión
de dos updates de una misma session) se prueba sin base de datos y sin esperar
a nada.

- **Ceiling**: 50 000 entradas (`DefaultSessionWindowSize`), configurable con
  `-session-window` / `ERRTRACK_SESSION_WINDOW`. Es un presupuesto de memoria,
  no una estimación de tráfico: una entrada son cuatro cadenas cortas y tres
  palabras de máquina.
- **TTL**: 1 hora. Al vencer, la session se cuenta con lo que se sabe de ella:
  empezó, y no dijo cómo terminó.
- **LRU intrusivo** (mapa + lista doblemente enlazada), porque el desalojo cae
  en la ruta de ingesta y tiene que ser O(1). Se desaloja **después** de
  insertar, nunca antes: una ventana llena que descartara el update entrante
  dejaría de contar sessions nuevas justo bajo carga, que es lo contrario de
  degradar con gracia.
- **Cuándo se cuenta**: al cerrarse (status terminal), al expirar o al ser
  desalojada. Los contadores resultantes se acumulan en un mapa `pending` y se
  escriben cada 60 s **y en el apagado** (`app.Serve` llama a `Health.Drain`
  después de drenar el servidor HTTP y antes de terminar).

### La imprecisión que se acepta, dicha entera

1. **Un reinicio pierde la ventana en vuelo.** Ya estaba en la decisión. Lo
   nuevo es que también se pierde lo *sentenciado y no escrito* si la parada no
   es limpia: hasta 60 s de contadores. Una parada limpia no pierde nada de eso
   — para eso existe el drain.
2. **Una session terminal se cuenta y se olvida.** No se guarda una lápida por
   session cerrada, así que un update posterior sobre ese mismo `sid` empieza
   una session nueva y se cuenta otra vez. La alternativa —recordar cada
   session ya sentenciada hasta el TTL— es memoria retenida por algo ya
   decidido, y bajo carga llenaría la ventana de muertas y desalojaría a las
   vivas: costaría precisión exactamente donde la ventana existe para
   protegerla. Los SDKs mandan los updates de una session en orden por una sola
   conexión, así que lo que se cede requiere un reintento para llegar
   desordenado.
3. **Al llenarse, una session desalojada puede contarse dos veces** si vuelve a
   hablar. Es la misma imprecisión que (2), provocada por el techo en vez de
   por el final.
4. **Los cuatro contadores son disjuntos** (`started = healthy + errored +
   crashed + abnormal`), como el propio item `sessions` del protocolo. Una
   session se cuenta una vez, bajo lo peor que le pasó.

Nada de esto es silencioso. Cada respuesta de health lleva un objeto `window`
con `in_flight`, `capacity`, `evicted`, `expired` y una frase —«figures for the
most recent hour may change after a restart»— que la CLI imprime y que la UI
muestra. Una sorpresa documentada no es una sorpresa.

### Medición (`scripts/health.sh`)

- 100 sessions por el SDK oficial de Python con `auto_session_tracking`, 5 de
  ellas crasheadas y 10 con un error manejado: `started=100`, `crashed=5`,
  `errored=10`, `healthy=85`, **`crash_free_rate = 0,95` exacto**.
- Una hora ya escrita es **idéntica byte a byte** tras un `kill -9`; las 65
  sessions que estaban en la ventana (40 sentenciadas sin flush + 25 en vuelo)
  desaparecen. Sólo la hora abierta se mueve.
- 1 000 sessions abiertas contra una ventana de 100: `in_flight` **nunca** pasa
  de 100, `evicted = 900`, el servidor sigue respondiendo, y las 900
  desalojadas **siguen contadas** como iniciadas. Precisión degradada,
  estabilidad intacta.
