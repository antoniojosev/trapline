# 009 — Driver SQLite puro Go (sin CGO)

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

Elegido SQLite (ADR 001), queda elegir driver. El canónico es un binding CGO sobre
la librería C oficial: maduro, rápido, es *el* SQLite. La alternativa es una
traducción del código C a Go puro.

La restricción que decide no es el rendimiento: es que **el producto es un binario
que se instala con `curl | sh` en linux/amd64 y linux/arm64**. Con CGO activo, la
compilación cruzada exige toolchains C por arquitectura, el binario deja de ser
estático y `goreleaser` pasa de configuración a proyecto de ingeniería. Además una
librería C en el proceso es superficie de memoria no gestionada en un servicio cuyo
endpoint de ingesta es público.

## Decisión

**`modernc.org/sqlite`**, puro Go, `CGO_ENABLED=0`.

## Consecuencias

- Binarios estáticos, cross-compile trivial, un `goreleaser` que es un fichero de
  config. Encaja con el presupuesto de tamaño de binario en CI.
- Es más lento que el binding C en microbenchmarks. El diseño ya lo absorbe:
  escrituras en batch y lecturas contra agregados, no consultas raw sobre millones
  de filas. **El gate de benchmark de ingesta en CI es el juez** — si el driver no
  sostiene el objetivo declarado de eventos/s, el gate lo dice y se revisa esta
  decisión con datos.
- Es una reimplementación: puede tener rarezas propias frente al SQLite oficial. Se
  mitiga testeando los adapters contra SQLite real (nunca mocks de base de datos) y
  no usando extensiones exóticas.
- Reversible sin tocar dominio: cambiar de driver es cambiar un adapter (ADR 004).
