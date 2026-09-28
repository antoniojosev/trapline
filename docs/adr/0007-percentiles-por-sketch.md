# 007 — Percentiles por sketch fusionable, nunca pre-calculados

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

El tracing necesita responder "p95 de este endpoint en las últimas 24 h" sin
escanear cada transaction. La solución obvia es pre-calcular por ventana: guardar
`p50`, `p95` y `p99` por minuto y por nombre de transaction.

**Esa solución obvia está mal.** Los percentiles no son componibles: no se pueden
promediar ni fusionar. Promediar los p95 de 60 minutos **no da** el p95 de la hora,
y con tráfico desigual la desviación es grande — un minuto con 3 requests lentos
pesa igual que un minuto con 10.000 rápidos. El dashboard mostraría números con
aspecto de correctos que no lo son, que es la peor clase de bug de observabilidad:
uno que nadie detecta y que hace tomar decisiones equivocadas.

## Decisión

Cada ventana guarda un **sketch de latencias fusionable** (t-digest o histograma de
buckets tipo HDR) como blob, junto a `count` y `fail rate`, que sí son componibles.
Los percentiles se **computan al momento de la query**, fusionando los sketches del
rango pedido.

**Nunca se persiste un percentil pre-calculado como número.**

## Consecuencias

- Esto está **en la frontera del motor** (ADR 004), no en el código de tracing: la
  agregación por ventanas tiene que soportar sketches desde el diseño. Descubrirlo
  al implementar tracing significaría rediseñar el motor con consumidores encima.
  Por eso se decide ahora y no cuando toque el tema.
- Los percentiles son aproximados con error acotado. Para "¿qué endpoint se puso
  lento tras el deploy?" es exacto de sobra; nadie necesita el p95 al microsegundo.
- Un sketch por ventana ocupa más que tres floats. Se compensa con downsampling: las
  ventanas viejas se fusionan a granularidad más gruesa, y fusionar sketches es
  precisamente la operación que estos soportan.
