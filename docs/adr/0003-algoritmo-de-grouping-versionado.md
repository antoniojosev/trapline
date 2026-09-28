# 003 — El algoritmo de grouping se versiona

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

El grouping — decidir que 10.000 ocurrencias son *un* issue — **es** el producto. Un
grouping malo se percibe como un producto malo, sin importar lo demás.

Y tiene una propiedad incómoda: cambiar el algoritmo **re-agrupa los issues de todo
el mundo**. Los issues resueltos reaparecen, los contadores se reinician, el
histórico se rompe. Un "arreglo" del grouping es un evento destructivo para quien ya
lo usa.

## Decisión

**El algoritmo se versiona explícitamente** y su versión se persiste con cada
fingerprint. Cambios de comportamiento solo en releases mayores, con migración
documentada.

El algoritmo: normalización del stacktrace (frames in-app vs librería;
módulo+función+archivo, **nunca número de línea**) y hash encadenado. Fallback: tipo
de excepción + mensaje normalizado (números, UUIDs y hex fuera). Un `fingerprint`
custom enviado por el SDK se respeta tal cual, sin tocarlo.

## Consecuencias

- Hay que convivir con decisiones de grouping imperfectas hasta la siguiente mayor.
  Es el precio de no romperle el histórico a nadie.
- **Corpus dorado de stacktraces reales multi-lenguaje como suite de tests.** Es el
  activo de calidad central del proyecto.
- El corpus se **siembra antes** del dogfooding — si no, hay un huevo-gallina:
  el grouping se construye antes de tener errores reales que lo validen. Fuentes de
  siembra: stacktraces sintéticos generados con los SDKs reales de la matriz
  (excepciones anidadas, async, código minificado), errores públicos de issues de
  GitHub, y errores históricos de producción de apps propias.
- Excluir el número de línea del hash es deliberado: si no, cada refactor que mueve
  código crea issues nuevos y falsea la señal de regresión.
