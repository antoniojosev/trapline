# 034 — El servidor escucha por familia de direcciones, no por lo que Go decida

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

`app.Serve` usaba `http.Server.ListenAndServe`, que siempre pide a Go la red
`"tcp"` y acepta lo que le devuelva. En una máquina dual-stack eso es **un**
socket atado al comodín IPv6 con `IPV6_V6ONLY=0`, que además acepta conexiones
IPv4 traducidas. Para cualquier cliente que se conecte, es indistinguible de
tener las dos familias.

No lo es para nada que mire *a qué familia está atado* un socket en vez de
conectarse a él. Y hay algo que lo mira: el relay de localhost de Docker
Desktop sobre WSL2 replica **sólo** los sockets atados a IPv4. Un servidor
arrancado con `-addr :9100` queda inalcanzable desde todos los contenedores de
la máquina mientras `ss` lo reporta escuchando tan tranquilo:

```
LISTEN 0 4096   *:9100   *:*          # ss dice que sí
wget: can't connect to remote host (192.168.65.254): Connection refused
```

La matriz de compatibilidad completa (`compat.sh --tier full`) falla entonces
con **«no address this machine offers was reachable from a container»**, una
frase que no apunta ni de lejos a una familia de direcciones. Un
`python -m http.server --bind 0.0.0.0` en el mismo puerto, en la misma máquina,
en el mismo segundo, sí es alcanzable — porque ata IPv4.

Hay un segundo caso, opuesto y ya documentado en `compat.sh`: en algunas
instalaciones de Docker, `host.docker.internal` resuelve a una dirección IPv6, y
un servidor atado a `0.0.0.0` es invisible **exactamente** para esos
contenedores. Las dos mitades son reales y tiran en direcciones contrarias, así
que ninguna familia sola es la respuesta.

Y hay un tercer problema, más pequeño y más viejo: `-addr 0.0.0.0:9000` pedía
IPv4 explícitamente y recibía un socket que `ss` reporta como `*:9000`. Go
ensancha la petición en silencio.

## Decisión

**La dirección se lee como lo que dice, y cuando no dice nada se abren las dos
familias.**

`internal/app.listen(addr)` devuelve una lista de listeners:

- **Literal IPv4** (`0.0.0.0:9000`, `127.0.0.1:9000`) → un socket `tcp4`. El
  operador pidió IPv4; recibe IPv4.
- **Literal IPv6** → un socket `tcp6`, por la misma razón.
- **Nombre de host** → `tcp`, y que resuelva lo que tenga que resolver.
- **Sin host** (`:9000`) → **dos** sockets, uno por familia. «Todas las
  interfaces» pasa a significarlo literalmente en vez de esperanzadamente.

El socket IPv4 se abre primero, y el orden es parte de la decisión: Go pone
`IPV6_V6ONLY` en un listener `"tcp6"`, así que la pareja no colisiona — pero
sólo en ese orden, porque un socket IPv6 dual-stack ya sería dueño del puerto.

Que falte una familia no impide arrancar: una máquina con IPv6 deshabilitado o
un contenedor sin ruta IPv4 son normales, y el arranque sólo falla si no queda
ninguna. Los sockets se abren **antes** de lanzar las goroutines que sirven, así
que un puerto ocupado es un error que `Serve` devuelve en vez de uno que llega
por un canal cuando los jobs de fondo ya arrancaron.

## Consecuencias

- La matriz completa vuelve a poder correrse en la máquina del mantenedor. No
  era un gate relajado ni un gate roto: era un gate que no podía ejecutarse, que
  es la forma más silenciosa de no tener uno.
- `-addr 0.0.0.0:9000` deja de aceptar IPv6. Es lo que pide esa dirección, y
  quien quiera ambas familias tiene `:9000`, que ahora las da de verdad.
- Un despliegue detrás de systemd o de un proxy no cambia: ambos hablan con el
  puerto, no con la familia a la que está atado.
- El diagnóstico queda escrito donde se sufrió, en el comentario de `listen`,
  porque el síntoma («ningún contenedor te alcanza») y la causa («el socket está
  atado a la otra familia») no se parecen en nada y no volverán a costar una
  tarde.
