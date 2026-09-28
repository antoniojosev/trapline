# La demo

El producto entero en una vuelta: llega un error de producción, un agente lo
lee, escribe el test que falla, parchea, y el issue se cierra para la próxima
release — y la instancia que todavía nadie ha redesplegado sigue lanzándolo sin
reabrir nada.

Dos contenedores. Ni cola, ni almacén columnar, ni caché. Eso no es una
simplificación para la demo: es el producto.

```sh
cd demo
./demo.sh --no-agent     # la vuelta entera, con el paso del agente simulado
./demo.sh                # la vuelta entera, con el agente de verdad
```

## Qué hace, paso a paso

| paso | qué pasa | qué demuestra |
|---|---|---|
| 1 | copia desechable del servicio, con historia de git | los suspect commits necesitan un repo, no un fixture |
| 2 | `docker compose up trapline` | un contenedor, un fichero de estado |
| 3 | proyecto, tracing encendido, release y commits | el tracing está apagado por defecto y encenderlo es una línea (ADR 005) |
| 4 | tres pedidos que funcionan y uno que no | el bug es de los que pasan la suite de tests |
| 5 | el error ya está aquí | ingesta, grouping y release, sin tocar un log |
| 6 | `issues bundle` | **un** documento con la excepción, los frames, los breadcrumbs, la frecuencia y el commit sospechoso |
| 7 | el agente (o sus parches) | test que falla primero, después el arreglo, después la suite entera |
| 8 | `issues resolve -next-release` | la promesa de [ADR 012](../docs/adr/0012-releases-orden-y-resolucion-en-proxima-release.md) |
| 9 | redeploy a `checkout@1.1.0` | y el pod viejo se deja encendido a propósito |
| 10 | el mismo pedido, otra vez | 200 en la release nueva; 500 en el pod viejo, **contado y sin reabrir** |
| 11 | `transactions list` | el p95 del endpoint estaba ahí todo el rato, en el mismo binario |

El paso 10 es el que hay que mirar dos veces. Un `resolve` normal habría
reabierto el issue con el primer evento del pod que todavía no se redesplegó,
que es exactamente lo que parece un arreglo que no funcionó.

## El bug

`src/checkout.js` aplica códigos de descuento:

```js
function applyDiscount(amount, code) {
  const rule = DISCOUNTS[code];
  return amount - (amount * rule.percent) / 100;
}
```

Una campaña que termina se borra de `DISCOUNTS`, pero su código sigue
circulando —en el correo que la anunció, en una captura, en una web de
cupones—. Los pedidos sin código funcionan y los pedidos con un código vivo
también, así que **la suite de tests está verde**. Sólo rompe con un código que
ya no existe, que es la clase de bug que llega a producción.

El arreglo son tres líneas y está en
[`fix/0002-fix-unknown-discount-code-is-no-discount.patch`](fix/0002-fix-unknown-discount-code-is-no-discount.patch).

## Los dos modos

**`./demo.sh`** corre `claude -p "/fix-error <id>"` en la copia del servicio,
con la skill de [`skills/fix-error/SKILL.md`](../skills/fix-error/SKILL.md). El
agente recibe `TRAPLINE_URL` y `TRAPLINE_TOKEN` y nada más: lee el bundle por
MCP o por CLI, localiza el código, escribe el test y parchea. Es lo que se
graba.

**`./demo.sh --no-agent`** sustituye ese paso —y sólo ese— por los dos parches
de `fix/`: primero el test, que se ejecuta y **se ve fallar**, y después el
arreglo. Es lo que corre en CI cada noche.

Lo que `--no-agent` no comprueba, dicho en voz alta: no comprueba que un agente
sepa arreglar este bug. Comprueba que todo lo que el agente necesita está ahí y
que todo lo que pasa después funciona, que es la parte que se pudre sola.

## Grabarla

1. `git status` limpio y nada escuchando en 9960–9962. La demo no escribe en la
   copia de trabajo: copia el servicio a un directorio temporal, le da historia
   de git ahí y lo parchea ahí, así que la tercera toma empieza exactamente
   igual que la primera.
2. Terminal a 100×32 aproximadamente, tema oscuro, fuente grande. La salida está
   pensada para leerse en vídeo: cada paso imprime el comando antes de
   ejecutarlo.
3. Una primera pasada con `--no-agent` para calentar la caché de Docker y de
   npm. En frío se van dos o tres minutos construyendo imágenes, y eso no es lo
   que hay que enseñar.
4. La toma buena, con el agente. Si el agente tarda, se corta ahí y se retoma:
   el paso 8 en adelante no depende de cómo llegó el parche.
5. El panel en `http://127.0.0.1:9960` durante los pasos 5 y 10 es la otra mitad
   de la historia, y es donde se ve el `suppressed N events from the resolved
   release`.

## Lo que la demo **no** enseña

- **Source maps.** El servicio es Node sin bundler, así que sus stacktraces ya
  son legibles y una demo de symbolication sería una demo sobre bundlers. Lo
  que eso hace está en [`../docs/adr/0018-source-maps-debug-ids-primero.md`](../docs/adr/0018-source-maps-debug-ids-primero.md)
  y tiene su propio gate (`scripts/sourcemaps.sh`, con el `sentry-cli` real).
- **Alertas, crons, uptime y status page.** Cada uno tiene su gate y ninguno
  cabe en la misma vuelta sin convertirla en un tour de funcionalidades.
- **Volumen.** Once transactions y dos errores no dicen nada de rendimiento.
  Eso es `make bench` y [`../docs/benchmarks/footprint.md`](../docs/benchmarks/footprint.md).

## Puertos

`9960` el servidor, `9961` el servicio redesplegado, `9962` la instancia vieja.
Se pueden mover con `PORT=`; el script aborta nombrando el puerto si alguno está
ocupado, en vez de hablar con lo que haya escuchando ahí.
