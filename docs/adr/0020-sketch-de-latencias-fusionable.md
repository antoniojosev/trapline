# 020 — Sketch de latencias: histograma logarítmico fusionable en `engine/sketch`

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

El ADR 007 ya decidió lo importante y lo decidió antes de que hubiera tracing:
**los percentiles no son componibles**. Promediar los p95 de sesenta minutos no
da el p95 de la hora, y con tráfico desigual la desviación es enorme — un
minuto con tres peticiones lentas pesaría lo mismo que un minuto con diez mil
rápidas. Guardar `p50/p95/p99` como números produciría un dashboard con cifras
de aspecto correcto que no lo son, que es la peor clase de bug de
observabilidad: el que nadie detecta y que hace tomar decisiones equivocadas.

Lo que el ADR 007 dejó abierto es **qué estructura** guarda cada ventana. Decía
«t-digest o histograma de buckets tipo HDR», que son dos cosas distintas con
propiedades distintas, y este ADR es el que tiene que elegir.

Hay además una decisión de ubicación. La agregación por ventanas con sketches
es una propiedad del **motor** (ADR 004), no del código de tracing: si el motor
no soporta sketches desde el diseño, descubrirlo al implementar tracing
significa rediseñarlo con consumidores encima.

## Decisión

Implementación propia, sólo stdlib, tipo **DDSketch**, en
`internal/engine/sketch`.

- **Mapeo**: buckets logarítmicos con `gamma = (1+α)/(1−α)` y **α = 1 %**. Un
  valor cae en el bucket `ceil(log_gamma(v))`, que cubre `(gamma^(i-1),
  gamma^i]`, y el bucket reporta `2·gamma^i/(gamma+1)` — el punto que está a
  menos de α relativo de sus dos extremos. La garantía es «el cuantil devuelto
  está a ≤ 1 % relativo del valor en ese rango del sample exacto».
- **Buckets dispersos**: `map[int32]uint64` en memoria. Un endpoint real ocupa
  unos cientos de buckets, no los 70 000 del rango representable.
- **Exactos, aparte de los buckets**: `count`, `sum`, `min` y `max` se guardan
  tal cual. `min` y `max` acotan el cuantil devuelto, de modo que un p99 nunca
  puede quedar por encima de la petición más lenta que ocurrió de verdad — la
  única cifra que un lector comprobaría de inmediato contra sus propios logs.
- **Serialización canónica**: un byte de versión, `count`/`zeros` como uvarint,
  `sum`/`min`/`max` como float64 little-endian, y los pares `(índice, cuenta)`
  ordenados con el índice **delta-codificado** en varint. Una distribución real
  ocupa una tirada contigua de índices, así que casi todos los deltas son 1 y
  ocupan un byte. Medido: 20 000 valores log-normales caben en **860 bytes**
  (0,043 bytes por valor); una distribución bimodal, en 265.
- **Operaciones**: `Add`, `AddN`, `Merge`, `Quantile`, `MarshalBinary`,
  `UnmarshalBinary`, más `Count/Sum/Min/Max/Mean`.
- **Convención de rango, escrita en el godoc**: `q` mapea al elemento
  `floor(q·(count−1))`, base cero. Está escrita porque cualquier test que
  compare contra un percentil exacto tiene que usar la misma, y «desplazado un
  elemento» y «el estimador está roto» producen el mismo mensaje de error.

### Por qué DDSketch y no t-digest

Tres razones, en el orden en que pesaron:

1. **La fusión es exacta.** Dos sketches con el mismo `gamma` tienen los mismos
   buckets, así que fusionar es sumar cuentas: el resultado es bit a bit el
   sketch que habría producido la unión de las entradas. La fusión de un
   t-digest es una aproximación de una aproximación, y el downsampling la
   aplica una y otra vez — cada hora del histórico tendría una forma
   ligeramente distinta según el orden en que se plegaron sus minutos.
2. **Es determinista.** Los mismos valores en cualquier orden dan los mismos
   buckets, que es lo que permite que un gate afirme una cifra exacta.
3. **Cabe en una pantalla.** Un índice, una cuenta y un mapeo de una línea.

### Ubicación y frontera

Vive **dentro de `internal/engine`** y no importa nada del resto del repo. Lo
verifican dos gates independientes: el test de arquitectura
(`internal/arch`, que recorre el directorio entero) y la regla `depguard`
`engine-is-standalone`. Es frontera del motor por el ADR 004 y por el 007: el
motor tiene que saber agregar ventanas con sketches, y el código de tracing es
un consumidor de eso, no su dueño.

## Consecuencias

- **Nunca se persiste un percentil.** `txn_minute` y `txn_hour` guardan
  `count`, `failed`, `sampled` y el blob; los percentiles se calculan al
  consultar fusionando el rango (ADR 021).
- Los percentiles son aproximados con error acotado al 1 %. Para «¿qué endpoint
  se puso lento tras el deploy?» sobra; nadie necesita el p95 al microsegundo.
- `UnmarshalBinary` lee bytes que vienen de disco, así que valida todo en vez
  de confiar: versión, rangos de índice, orden ascendente, buckets vacíos, y
  **que las cuentas de los buckets sumen el total de la cabecera**. Esa última
  no es sobre la codificación: un sketch cuyo `count` no cuadra con sus buckets
  reportaría percentiles de una distribución y un ritmo de otra, y nada aguas
  abajo podría notarlo. Está fuzzeado (`FuzzUnmarshalBinary`, en CI).
- Cobertura del paquete: **99,4 %**. El único bloque sin cubrir es un `return`
  inalcanzable salvo por redondeo en coma flotante al final del rango.

## Medición

Reproducible con `go test ./internal/engine/sketch/`:

| distribución | valores | buckets ocupados | bytes codificados |
|---|---|---|---|
| uniforme (1–1000 ms) | 20 000 | 310 | 709 |
| log-normal | 20 000 | 399 | 860 |
| bimodal (cache hit/miss) | 20 000 | 94 | 265 |

Error relativo frente al percentil exacto en las tres, para
q ∈ {0, 0,5, 0,75, 0,9, 0,95, 0,99, 1}: **≤ α en todos los casos**.

Fusionar 60 sketches parciales da el mismo mapa de buckets, la misma cuenta,
los mismos extremos y los mismos cuantiles que un sketch de la unión — en los
dos órdenes de plegado.
