# 017 — Página de estado pública renderizada en el servidor

- **Fecha**: 2026-08-29
- **Estado**: Aceptada

## Contexto

Los monitores de uptime saben si algo responde. Ese dato sólo sirve dentro del
panel, detrás de un login, para las mismas dos o tres personas que ya se van a
enterar por la alerta. La pregunta que no tiene respuesta es la de fuera: un
cliente al que se le rompió algo quiere saber si es él o es el servicio, y hoy
tiene que preguntar.

La pantalla que responde eso se lee **cuando algo está roto**. Eso decide casi
todo lo demás: se abre desde un móvil, desde una conexión de la que su dueño ya
está harto, y a menudo desde una red distinta de la que falla. Una página que
necesita descargar un bundle de React de 280 KB para decir «todo bien» es una
página que, el día que hace falta, dice una rueda girando.

Y es la única superficie de este producto que lee alguien que no tiene cuenta.

## Decisión

**`GET /status/{project_slug}`, HTML renderizado en el servidor, servido sólo
si el proyecto lo activó.**

- **`html/template` embebido, sin React y sin JavaScript obligatorio**, con el
  CSS en línea. No hay un segundo fichero que pueda no llegar; no hay
  superficie de script que atacar; el escapado por contexto de `html/template`
  es lo que hace seguro imprimir lo que un operador escribió.
- **Se activa por proyecto** (`projects.config.status_page.enabled`) y muestra
  **sólo los monitores marcados `public`**. Publicar es una decisión sobre un
  servicio concreto: una instalación normal tiene uno de cara al cliente y
  varios internos, y «todo lo que vigilo sale en una página» no lo quiere
  nadie. Un monitor que nadie marcó es el nombre de un servicio interno.
- **Título y descripción viven en `settings`**, para toda la instalación:
  nombran a la organización y no al proyecto, así que se escriben una vez. Sin
  título, cada página usa el nombre de su proyecto.
- **Las cifras salen de `uptime_daily` y de ningún otro sitio.** Noventa días
  de un monitor de sesenta segundos son ciento treinta mil filas de checks, y
  una página pública que las recorra es exactamente la consulta que existe
  ADR 001 para prohibir. La resolución del agregado es el día, así que una
  ventana móvil —«24 h»— cuenta el día del borde **en proporción** a la parte
  que la ventana cubre. Es una aproximación y se dice en la propia página; la
  barra de debajo es por día y es exacta.
- **Incidentes = días con fallos**, con el número de checks caídos y una
  duración aproximada a partir del intervalo del monitor. Un rango exacto
  exigiría recorrer los checks, que es lo que no se hace.
- **Caché en memoria de 30 s por proyecto**, `Cache-Control: public,
  max-age=30`. Treinta segundos es el suelo del intervalo de un monitor: una
  caché más corta no puede enseñar nada más nuevo porque no existe nada más
  nuevo. **Un fallo no se cachea**: la clave la elige quien escribe la URL.
- **Sin autenticación y con límite por IP**, el mismo limitador y el mismo
  techo que la ingesta y el ping (ADR 023).
- **Un proyecto que no publica responde 404, igual que uno que no existe.** La
  diferencia entre los dos es la decisión del operador de no publicar.

## Consecuencias

- La página dice **cuándo se generó**. Una página cacheada que no dice su edad
  invita a leer una caída de hace treinta segundos como el estado del mundo.
- **Una ventana sin checks no es «100 %»**, es «—». Y un día anterior a la
  creación del monitor se dibuja en gris y no en verde: dibujar noventa días
  verdes detrás de un monitor creado esta mañana sería una afirmación de tres
  meses de disponibilidad que ningún dato respalda. Es la deuda que uptime dejó
  escrita y que la status page cierra; el dato que la resuelve es `CreatedAt`.
- **Nunca se redondea hacia arriba hasta 100 %.** 99,996 % se imprime
  `99.99 %`.
- **Una página sin monitores públicos no dice «todo bien»**, dice que no se
  está informando de nada. Decir que todo va bien sin evidencia es lo único
  que una página de estado no puede hacer.
- **Un checkeo desde un solo sitio.** Un binario, una máquina: la página lo
  dice en el pie en vez de fingir consenso entre regiones.
- El interruptor viaja en `projects.config`, que es un documento JSON leído en
  la ruta caliente de ingesta. Es un booleano y no cuesta nada medible; lo que
  compra es no tener una migración y una columna para él.

## Medición

Tomada el 2026-08-29, sobre `scripts/uptime.sh`
ampliado (nginx en contenedor, checks reales, el monitor ya caído y recuperado
antes de que la página se lea).

| Qué | Valor |
|---|---|
| Dependencias nuevas | ninguna: `html/template` y `embed` son stdlib |
| Tamaño del binario | 16 MB / 30 MB de presupuesto |
| Peso de la página | ~9 KB, un único documento, cero peticiones adicionales |
| Consultas por render | 1 proyecto + 1 config + 1 listado + 1 por monitor público |
| Consultas con caché caliente | cero: una búsqueda en un mapa |
| 200 lecturas seguidas | 0 rechazadas (gate) |
| Cobertura `domain/status_page.go` | 100 % salvo `overlapShare` (92,3 %) |
| Cobertura `usecase/statuspage.go` | 86–100 % por función |
