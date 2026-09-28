# 006 — Una sola API, cuatro clientes

- **Fecha**: 2026-08-22
- **Estado**: Aceptada

## Contexto

Hay cuatro formas de operar esto: dashboard web, CLI, MCP (para agentes) y consumo
programático desde otro sistema. El camino habitual es hacer la web primero y luego
añadir una CLI que la imita — y la CLI acaba siendo ciudadana de segunda, con la
mitad de las operaciones y comportamiento inconsistente.

Para un producto cuyo diferencial es ser operable por un agente, eso es fatal: el
agente usa exactamente los caminos que el desarrollo trata como secundarios.

## Decisión

**Los casos de uso se implementan una vez, en el dominio, y se proyectan a cuatro
adapters del mismo puerto hexagonal**: REST, web UI, CLI y MCP. Ningún adapter tiene
lógica de negocio; ninguna operación existe en uno y falta en otro.

Contrato de la CLI, porque es el que consume un agente: `--json` en todos los
comandos, cero prompts interactivos, exit codes estables y documentados, comandos
idempotentes.

## Consecuencias

- Un feature nuevo no está terminado hasta estar en los cuatro. Es más lento por
  feature y elimina la deuda de paridad, que es la que nunca se paga.
- La API REST se documenta desde el principio pero **se congela más tarde**: es
  `v1-beta` y mutable durante el desarrollo temprano, y se congela como contrato
  público cuando haya información real de uso y el primer consumidor externo a la
  vista. Congelarla en la semana 8, antes de que nadie la haya usado, sería
  comprometerse a ciegas con las decisiones peor informadas del proyecto.
- Una vez congelada, esa API es la frontera de integración de todo lo externo,
  incluido **el control plane del servicio hosteado, que es simplemente un cliente
  más** — vive fuera de este repo, no condiciona su licencia y no puede tomar atajos
  por dentro del binario.

## Anexo — la congelación, 2026-09-20

Esta sección no cambia la decisión: **registra que la condición que ella misma
puso ya se cumplió.** La decisión decía "se congela cuando haya información real
de uso"; esto dice cuándo fue y qué significa exactamente a partir de ahora.

**Qué se congeló.** `/api/v1-beta/` pasó a `/api/v1/` el 2026-09-20, con 68 rutas
versionadas y tres superficies públicas sin versión (ingesta, `/ping/`,
`/status/`). El documento es `docs/api/openapi.yaml`, escrito a mano.

**Por qué ahora y no antes.** La información de uso existe: la superficie entera
la recorren los gates de extremo a extremo, la consumen tres clientes propios
—panel, CLI y los verificadores de `compat/`— y dos SDK ajenos, y la forma dejó
de moverse. Lo que faltaba no era tiempo, era evidencia, y la evidencia es que
las últimas cuatro fases añadieron rutas y no renombraron ninguna.

**Qué significa "congelada", en términos ejecutables.** Se puede **añadir** una
ruta; una respuesta puede **ganar** un campo; una petición puede ganar un campo
**opcional**. No se puede quitar, renombrar ni cambiar el significado de nada
bajo `/api/v1/` sin un prefijo de versión nuevo. Quien lo vigila es
`TestRoutesMatchOpenAPI`, que compara la tabla de rutas de
`internal/adapters/httpapi/routes.go` con el documento en las dos direcciones:
una ruta servida y no documentada falla, y una documentada que ya no se sirve
falla también — y la segunda es la peor de las dos, porque es un contrato
publicado que miente.

**El alias.** `/api/v1-beta/` sigue respondiendo **una minor**, con
`Deprecation: true` y `Link: </api/v1>; rel="successor-version"`. No para
siempre: un alias que nunca anuncia su final es una segunda API permanente, que
es justamente lo que congelar la primera pretendía evitar.

**Lo que no se congela.** La superficie `/api/0/` que habla `sentry-cli` no
entra aquí y no lleva versión. No es la API de este producto: es el protocolo de
otro, implementado a partir de tráfico grabado y especificado por
`compat/sentry-cli/fixtures/` (ADR 013). Congelarla sería atribuirse la autoría
del contrato ajeno, y documentarla en este OpenAPI le diría a quien lo lea que
es una superficie sobre la que puede construir. No lo es.
