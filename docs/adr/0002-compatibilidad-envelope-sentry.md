# 002 — Compatibilidad con el protocolo envelope + tolerancia a items desconocidos

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

La barrera de entrada real de un producto de error tracking no es el backend: son
los SDKs. Hay que instrumentar decenas de lenguajes y frameworks, y mantenerlos.
Ningún competidor de un solo mantenedor gana esa carrera.

Los SDKs oficiales de Sentry hablan un protocolo estable y documentado (el
*envelope*), y el endpoint al que hablan se configura con **una sola línea**: el DSN.

## Decisión

**Implementar el endpoint envelope y ser compatible con los SDKs oficiales.**
Migrar es cambiar el DSN; no se reescribe instrumentación.

`POST /api/{project_id}/envelope/`, auth por `X-Sentry-Auth` (sentry_key del DSN) o
DSN en el header, bodies gzip/zstd. Tipos de item en v1: `event`, `transaction`,
`session`/`sessions`, `check_in`, `client_report`.

**Los items desconocidos se ignoran con log; jamás se rechaza el envelope
completo.** Esa tolerancia es lo que hace que un SDK que evoluciona no rompa una
instalación vieja.

## Consecuencias

- Se hereda una superficie de decisiones ajenas y se depende de que el protocolo no
  derive. Mitigación: parser tolerante + matriz de compatibilidad en CI con
  versiones pinneadas y actualización mensual programada.
- El parser de envelopes es un **paquete puro y fuzzeado en CI**: es la superficie
  que recibe input hostil de internet.
- La matriz de compatibilidad corre en **dos niveles**: smoke rápido en cada PR
  (JS/Node, Python, Go) y matriz completa (browser, PHP/Laravel, Flutter) en nightly
  y como gate de release. Una matriz completa por PR sería lenta y flaky, y acabaría
  desactivada — que es peor que no tenerla.
- La matriz publicada en el README es una feature de marketing, no solo un test.
- No se usa la marca ajena para nombrar el producto: la compatibilidad se describe
  de forma nominativa.

## Medición (2026-08-29)

Anexo, no revisión: la decisión de arriba no cambia. Se registra aquí lo que la
matriz completa encontró al cerrarse con los seis SDKs, porque el ADR afirmaba
cosas que hasta ahora nadie había ejecutado.

**La matriz encontró cinco bugs, ninguno visible desde los tests propios.** Tres
ya estaban documentados (Go, Python, Node). Los dos nuevos los encontró el
navegador, y los dos son del tipo que este ADR predijo al decir que se hereda
"una superficie de decisiones ajenas":

1. **La ingesta no servía política CORS.** Un POST cross-origin con body plano
   no necesita preflight, así que el navegador enviaba todos los eventos, el
   servidor los guardaba y respondía 200, y nada parecía roto — mientras el
   navegador impedía a la página leer una sola respuesta. En la respuesta va la
   contrapresión del protocolo: `X-Sentry-Rate-Limits` es cómo este servidor le
   dice a un SDK que deje de mandar una categoría apagada, y es la razón entera
   de que un subsistema apagado no cueste **en el cable** y no solo en disco
   (ADR 005). Todo SDK de navegador era sordo a eso. Corregido permitiendo
   cualquier origen en ingesta — y solo en ingesta, que no tiene credencial
   ambiente — y exponiendo las cabeceras de límite.

2. **Un error de navegador no registraba en qué navegador ocurrió.** El SDK no
   puede ayudar: una página no tiene un nombre fiable de sí misma. El dato está
   en la única cabecera que escribe el navegador en cada petición, y se estaba
   tirando. Ahora `User-Agent` se lee a `contexts.browser` en la ingesta.

**Los dos niveles de CI de este ADR quedan ejecutables.** `--tier smoke` (Go,
Python, Node; segundos, sin contenedores) en cada PR;
`--tier full` (+ navegador, PHP, Dart; minutos, cuatro imágenes) en
`nightly.yml`. La lista de "browser, PHP/Laravel, Flutter" del texto original
se cumplió como navegador, PHP y **Dart**: `sentry_flutter` envuelve el paquete
Dart y comparte transporte y forma de evento, así que lo que ve el servidor es
lo mismo y un emulador en la matriz no probaría nada nuevo.

**La tolerancia a items desconocidos se sostuvo.** Los seis SDKs mandan cosas
que este build no almacena — sesiones sin pedirlas los tres de JS y Python,
`client_report` el navegador — y ninguna rompió un envelope.

**Lo que la matriz no puede prometer.** Un frame minificado llega como
`Object.t [as decode]`, y el nombre mangleado cambia en cada build: el mismo bug
puede recibir identidad nueva en cada despliegue. La regla de nombres de asset
con hash cubre el fichero; nada cubre la función. Es el motivo de existir de los
source maps, y la suite de navegador ya deja construido el bundle minificado con
su mapa al lado como fixture de ese trabajo.
