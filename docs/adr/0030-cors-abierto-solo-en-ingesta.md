# 030 — CORS abierto en ingesta, y en ningún otro sitio

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

La suite de navegador de la matriz de compatibilidad encontró que el
endpoint de ingesta no servía ninguna política CORS, y que el fallo era
invisible desde el servidor.

Un POST cross-origin con body plano es una petición *simple*: el navegador la
envía permita o no el servidor ese origen, y solo impide a la página **leer la
respuesta**. Así que los eventos llegaban, se guardaban, el log decía 200 y
nada parecía roto. Lo que se perdía era la respuesta.

En la respuesta está la contrapresión del protocolo. `X-Sentry-Rate-Limits` es
cómo este servidor le dice a un SDK que deje de mandar una categoría apagada, y
que el SDK la honre es la razón entera de que un subsistema apagado no cueste
**en el cable** y no solo en disco (ADR 005). Un SDK de navegador que no puede
leer esa cabecera sigue pagando el precio completo para siempre. Y cualquier
configuración de SDK que provoque preflight — una cabecera propia, un
content-type explícito, un tunnel — no habría entregado nada, sin rastro en
ningún lado.

Esto no es un detalle de una plataforma: el navegador es el cliente que la
promesa de compatibilidad (ADR 002) más necesita, porque es donde vive el
código que más se rompe.

## Decisión

**La ingesta responde `Access-Control-Allow-Origin: *`, expone las cabeceras de
límite y contesta el preflight. Ningún otro endpoint sirve cabecera CORS
alguna.**

- Ingesta: `Allow-Origin: *`, `Expose-Headers: X-Sentry-Rate-Limits,
  Retry-After, X-Sentry-Error`, `OPTIONS` respondido con los métodos y
  cabeceras que usan los transportes de los SDKs, y `Max-Age` de un día.
- **Sin credenciales.** `*` y `Allow-Credentials: true` son incompatibles por
  especificación, y pedirlas significaría que este endpoint se puede alcanzar
  con la cookie de sesión de alguien adjunta.
- La API del panel no sirve ninguna cabecera CORS, y hay un test que lo
  verifica.

Cualquier origen es lo **correcto** aquí, no lo laxo, y por la misma razón por
la que este endpoint tampoco exige la cabecera anti-CSRF: no hay credencial
ambiente que abusar. La autenticación es una clave que el llamante presenta
explícitamente y que ya viaja dentro de bundles públicos. Una lista de orígenes
no protegería nada — quien tiene la clave tiene la clave — y rompería el caso
normal de una instalación recibiendo eventos de varias aplicaciones, que es la
razón de ser de un servidor multi-proyecto.

## Consecuencias

- Cualquier página puede enviar eventos a un proyecto cuya clave conozca. Eso
  ya era cierto antes de esta decisión: la clave es pública por diseño y las
  defensas reales son las que ya existen — límite de tamaño, rate limit por
  proyecto, scrubbing, y una clave que solo permite escribir en su proyecto
  (SECURITY.md).
- La política queda pegada al router de ingesta, no al middleware global. Un
  endpoint nuevo no hereda CORS por accidente; hay que pedirlo.
- Si algún día la ingesta necesitara aceptar credenciales — no se ve el caso —
  esta decisión se supersede, porque `*` dejaría de ser válido.
- La suite de navegador comprueba la parte que un SDK no puede comprobar de sí
  mismo: manda un envelope de una categoría apagada desde la página y exige un
  429 legible con su cabecera legible. Sin esa prueba, el fallo vuelve a ser
  invisible.
