# 038 — El presupuesto de artefactos se cobra donde se conoce el proyecto

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación
- **Contexto de origen**: la subida de artefactos de source maps. ADR 018 fija que hay un presupuesto por
  proyecto y que se responde `413`; no dice **dónde**, y la grabación demuestra
  que el sitio obvio no existe.

## Contexto

ADR 018 dice: «Presupuesto por proyecto (`config.artifacts_max_mb`, default
200) → `413` al exceder». Las dos mitades de esa frase resultan ser
incompatibles con el protocolo real, por razones distintas.

**El sitio donde hay bytes no sabe de qué proyecto son.** El endpoint que
recibe los chunks es
`POST /api/0/organizations/{org}/chunk-upload/`, y la grabación es tajante: no
lleva proyecto en la ruta, ni en la query, ni en el cuerpo — el cuerpo es
`multipart/form-data` con un part por chunk y nada más, según la grabación
del tráfico de `sentry-cli`. Se sabe cuántos bytes están
entrando y no se sabe a quién cobrárselos.

**El sitio donde se sabe el proyecto no puede contestar `413`.** El proyecto
aparece en el ensamblado (`projects: ["venekambio"]` en el cuerpo), pero ese
endpoint no lee códigos de estado como errores: su contrato es un objeto con
`state`, y lo medido es que un `413` ahí llega al usuario como *"unknown
error"*, medido. Un rechazo que no dice qué cambiar no es un rechazo,
es una caída.

Y hay una tercera cosa que sí necesita defensa: un cliente puede subir chunks
indefinidamente sin ensamblar nunca. Sin techo, el área de staging es una forma
de llenar el disco con una credencial de escritura y ninguna intención de
completar nada.

## Decisión

**Dos límites distintos, cada uno donde se puede aplicar y con la forma que su
interlocutor sabe leer.**

1. **Techo de instalación sobre el área de staging**, cobrado en
   `POST …/chunk-upload/`, que es donde hay bytes. Es installation-wide porque
   esa petición no nombra proyecto, y se responde con el código de estado,
   porque en el `POST` de chunks la herramienta sí lee el código.
2. **Presupuesto por proyecto** (`artifacts_max_mb`), cobrado en el
   **ensamblado**, que es donde el proyecto se conoce, y reportado **dentro del
   cuerpo** como `{"state":"error","detail":"…"}`. El `detail` es lo que la
   herramienta imprime, así que nombra el uso actual, el techo y el ajuste que
   lo mueve.
3. En las superficies que **sí** nombran proyecto y **sí** leen códigos —la
   subida legacy de un fichero de release y la API propia de este producto— el
   mismo exceso responde **`413`**, que es lo que decía ADR 018 y sigue siendo
   correcto ahí.
4. Los bytes que una re-subida **reemplaza** se descuentan antes de comparar.
   Un pipeline que despliega el mismo build dos veces no lo cuenta dos veces.
5. `artifacts_max_mb` es un puntero en la configuración, no un entero: **cero
   es una decisión** —«este proyecto no envía JavaScript y nadie va a subir
   megabytes a su nombre»— y no una ausencia. Es la lección de ADR 031 aplicada
   a un techo en lugar de a una ventana.

## Alternativas descartadas

- **Cobrar todo en el ensamblado y no poner techo al staging.** Deja abierta la
  forma más barata de llenar el disco: subir chunks y no ensamblar nunca.
- **Cobrar el presupuesto por proyecto en el `POST` de chunks igualmente,
  adivinando el proyecto.** No hay de dónde: el único candidato sería el token,
  y un token de este producto tiene alcance sobre todos los proyectos de la
  instalación. Adivinar significaría cobrarle a un proyecto lo que subió otro.
- **Contestar `413` en el ensamblado.** Es lo que dice ADR 018 literalmente, y
  lo medido es que sale como *"unknown error"*: el usuario ve que falló y no
  ve por qué.
- **Rechazar en el ensamblado antes de leer el ZIP, contando los bytes del
  bundle.** Contaría comprimido lo que el presupuesto mide descomprimido, y un
  bundle de source maps comprime muchísimo: el techo real sería varias veces el
  configurado, distinto para cada proyecto, y nadie podría explicarlo.

## Consecuencias

- Hay dos números y hay que saber cuál falló. El mensaje de cada uno lo dice:
  el del staging habla del área de subida de la instalación, el del proyecto
  nombra el proyecto y `artifacts_max_mb`.
- Un exceso se descubre **después** de haber subido los bytes, no antes. Es
  inevitable —el protocolo no ofrece ningún momento anterior en que se sepa el
  proyecto— y es barato: los chunks caducan solos
  (`domain.ChunkRetention`) y nada se escribió en `artifacts`.
- Un `artifacts_max_mb` de cero rechaza toda subida, incluida la primera. Es lo
  que se pidió, y `config show` lo enseña como un valor puesto y no como el
  default.
