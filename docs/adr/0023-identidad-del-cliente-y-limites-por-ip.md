# 023 — Identidad del cliente y límites por IP

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

`SECURITY.md` afirmaba dos cosas que el código no hacía:

1. *"Rate limit por IP además de por proyecto. Detrás de un reverse proxy, la
   confianza en `X-Forwarded-For` es configuración explícita."* No existía.
   `RemoteAddr` no aparecía ni una vez en el repo; `Config.TrustedProxies` se
   parseaba, se logueaba y no lo consumía nadie. El único 429 del producto era
   el del limitador por proyecto y categoría.
2. *"Panel tras login (admin único, argon2id, sesiones), con rate limit en
   auth."* Tampoco existía: `/setup` y `/login` no tenían ningún límite.

La segunda es la peligrosa. Cada intento de login cuesta un Argon2id de 19 MiB
de working set (ADR 005). El gate de memoria medía el reposo y el pico de **un**
login; nadie había medido mil. **Medido antes de este ADR: 200 logins
concurrentes llevaron la RSS del proceso a 1,03 GB** — treinta veces el
presupuesto publicado de 30 MB en reposo, contra un producto cuyo argumento de
venta entero es la huella. No hace falta autenticarse para conseguirlo, y no
hace falta ancho de banda: cabe en un `curl` en bucle.

El patrón es el que este proyecto lleva cazando desde el principio — una promesa
publicada que el código no cumple — y `SECURITY.md` es el peor sitio donde
tenerlo, porque es el documento que alguien lee para decidir si exponer esto a
internet.

## Decisión

### Quién es el cliente: paquete puro `internal/clientip`

Un paquete nuevo, sin `net/http`, sin reloj y sin configuración: lee dos
strings — el peer directo y lo que traía `X-Forwarded-For` — y devuelve una
dirección. Es una frontera de seguridad, y está en el gate de `internal/arch` y
en `depguard` junto a `domain`, `engine` y `envelope`.

La regla, completa:

- **Si el peer directo coincide con `TrustedProxies`** (se aceptan **IPs y
  bloques CIDR**, en ambas familias), se recorre `X-Forwarded-For` **de derecha
  a izquierda saltando los hops que también sean de confianza**, y se toma el
  primero que no lo sea.
- **Si el peer directo no es de confianza**, se usa `RemoteAddr` y se **ignora
  `X-Forwarded-For` por completo**. No "como pista", no "para el log":
  se ignora.
- **Sin lista configurada no se cree a nadie**: peer directo y punto.
- **Un hop ilegible corta el recorrido**: nada a su izquierda es verificable, y
  la respuesta cae al peer directo.
- Si **todos** los hops son de confianza, el de más a la izquierda es el
  origen y lo escribió un proxy que sí creemos: se devuelve ese. Devolver el
  peer ahí metería a todos los clientes internos en un solo bucket.

Un XFF creído sin comprobar quién lo envía no es información: es una lista de
IPs que el atacante elige, y por tanto un limitador cuyas claves elige él.

Se normaliza la dirección (IPv4-mapped desmapeada, zona descartada) para que
una misma máquina no ocupe varios buckets cambiando cómo se escribe.

### Dos limitadores, no uno reutilizado

- **Ingesta** — por IP, aplicado **antes de leer y decodificar el body**. El
  coste que se evita es el `io.Read` y el gunzip de hasta 20 MiB; un límite
  después de leer defiende la base de datos y deja sin proteger la mitad cara
  de la petición. Default **48 000/min por dirección**
  (`DefaultRateLimitPerMinute × IPProjectHeadroom`, con `IPProjectHeadroom = 4`),
  derivado de las constantes de diseño en `domain` en vez de inventado, y
  fijado por un test. **Generoso a propósito**: todo el tráfico legítimo de un
  backend sale de una sola IP, así que un límite estrecho aquí no para un
  ataque, para al cliente.
- **Autenticación** (`/setup` y `/login`) — por IP y **estricto**: **10/min**.
  Lo que se raciona no es ancho de banda sino 19 MiB por intento. Diez al
  minuto es generoso para el **único** humano que puede entrar (admin único),
  y ese es exactamente el argumento: aquí no hay una flota de usuarios que
  acomodar, así que el límite estricto no cuesta nada.

Ambos configurables (`-ingest-ip-rate-limit`, `-auth-rate-limit` y sus
variables de entorno).

### Un tope global de hashes en vuelo

Un límite por dirección no acota la memoria: mil direcciones cada una por
debajo de su techo llegan igual a la vez. Se añade un semáforo de
**2 hashes concurrentes** en las rutas que gastan Argon2id. Refusa, no encola:
una cola delante de una función memory-hard es una forma de retener la memoria
igual, sólo que más tarde. Con admin único, 2 es holgado.

### El limitador no puede ser él mismo el vector

Un `map[IP]contador` crece sin límite si el atacante rota direcciones, y dentro
de un /64 de IPv6 rotarlas es gratis. Dos medidas:

- **Se cuenta la /64 en IPv6** y la dirección exacta en IPv4. Sin esto un
  limitador IPv6 nunca ve dos veces la misma clave y es decorativo.
- **El mapa está acotado** (16 384 entradas para ingesta, 4 096 para auth) con
  desalojo por sondeo de 8 entradas: prefiere una caducada y si no tira una
  arbitraria. Un barrido completo sería O(n) en cada inserción justo en el
  estado al que un atacante lleva el mapa, y la limpieza se convertiría en la
  amplificación. **Al llenarse se sacrifica precisión, nunca estabilidad** — el
  mismo criterio que ADR 008 aplica a la ventana de sessions.

### Forma de la respuesta: 429 con `Retry-After` y nada más

Un 429 por IP **no** lleva `X-Sentry-Rate-Limits`. Ese header es el contrato del
protocolo para "esta categoría de este proyecto está apagada o pasada de
vuelta" (ADR 005), y los SDKs oficiales lo obedecen **dejando de enviar esa
categoría** durante la ventana. Decirle a un SDK sano que deje de mandar
errores durante minutos porque su IP hizo una ráfaga sería convertir una
defensa en pérdida de datos — y de exactamente los eventos que alguien está
esperando. Un 429 pelado con `Retry-After` es lo que un SDK trata como fallo
transitorio y reintenta. Esa diferencia es la razón de que sean dos
limitadores y no uno.

## Consecuencias

- **La huella publicada vuelve a ser cierta bajo ataque, no sólo en reposo.**
  Medido con 200 logins concurrentes: **1,03 GB antes, 52,9 MB después**. El
  `smoke.sh` ahora hace ese flood **justo antes** de medir el pico, así que la
  cifra del presupuesto de memoria es la de un servidor bajo ataque y no la de
  un servidor al que una persona acaba de entrar. Medir un solo login era el
  mismo error de categoría una capa más abajo: respondía "¿cuánto cuesta un
  login?" cuando la pregunta del atacante es "¿cuánto cuestan mil?".
- **`TrustedProxies` mal escrito ahora **para el arranque**, con la entrada
  ofensora nombrada.** Aceptarlo en silencio dejaba a un operador creyendo que
  había configurado algo que no, y el síntoma — cada visitante contado como el
  proxy — se parece exactamente a un rate limit que funciona.
- **La precisión del límite de ingesta se degrada bajo muchísimas direcciones
  distintas**, que es el caso legítimo de un SDK de navegador. Es aceptable: el
  techo por dirección es deliberadamente alto y quien de verdad protege el
  disco en ese escenario es el límite por proyecto.
- **Agrupar por /64 mete varios clientes reales de un mismo prefijo residencial
  en un bucket.** Es el precio de que el límite signifique algo en IPv6, y es
  asumible porque el techo es holgado.
- **Un despliegue detrás de un proxy que no configure `TrustedProxies` contará
  a todos sus visitantes como el proxy.** El arranque lo dice explícitamente a
  nivel `info`: "detrás de un proxy sin confiar en nadie" y "no hay proxy" son
  el mismo estado dentro del proceso y despliegues muy distintos.
- **Lo que esto NO protege**, dicho en `SECURITY.md` para que no vuelva a haber
  una promesa por delante del código: el resto de la API autenticada no tiene
  rate limit (sus credenciales son revocables), no hay límite por ancho de
  banda ni por bytes, y un atacante distribuido con suficientes direcciones
  sigue pudiendo saturar la red — lo que no puede es agotar la memoria, que es
  lo que el tope global acota.
- **El limitador queda listo para `/ping/` de crons**: engancharlo es
  una línea, `limitByIP(s.limits.ingest, s.clientAddr, deny…, handler)`.
