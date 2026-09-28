# 031 — Una retención de cero días significa no conservar nada

- **Fecha**: 2026-08-29
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

`ProjectConfig.RetentionDays` es un mapa de categoría a días. Hasta ahora,
`retentionFor` lo leía así:

```go
if days, configured := config.RetentionDays[string(category)]; configured && days > 0 {
```

Es decir: un cero explícito se trataba como "usa el valor por defecto". Eso produce
dos cosas indeseables a la vez:

1. **Un ajuste que no hace nada.** Un operador puede escribir `error=0`, leerlo de
   vuelta —la API lo devuelve tal cual— y observar que las barridas siguen borrando
   a los 90 días. La configuración dice una cosa y el sistema hace otra, sin error
   ni aviso.
2. **No hay forma de decir "no conserves nada".** El mínimo expresable era un día,
   y un día no es cero. Para un proyecto que sólo quiere el dashboard —los
   contadores, que viven 400 días (ADR 010)— y no quiere payloads en disco, no
   existía ninguna configuración que lo dijera.

El segundo punto es además lo que el gate de retención necesita poder expresar: la
propiedad que justifica los agregados es que sobreviven a los eventos, y probarla
exige borrar los eventos.

## Decisión

**Un cero explícito en `retention_days` significa "no conservar nada": el corte es
`ahora` y la siguiente barrida borra todo lo de esa categoría.** La forma de
heredar el valor por defecto es **omitir la categoría**, que es lo que la API
siempre documentó ("omit it for the default") y lo que el `PUT` ya hace, porque
sustituye el mapa entero.

Como el `PUT` sustituye el mapa, no había forma de volver a heredar *todo*: se
añade `-retention default` en la CLI, que envía `retention_days: null` y limpia
cada override. Es el mismo tercer estado que `-categories default`, por la misma
razón: el camino de vuelta es un estado propio, no la ausencia de los otros dos.

Los negativos se siguen rechazando.

## Consecuencias

- **Es un cambio de significado de un valor almacenado.** Un proyecto con un cero
  guardado hoy —inerte— empezaría a purgar esa categoría en la siguiente barrida.
  El producto es pre-1.0 y sin instalaciones, y el valor no puede haber sido puesto
  a propósito porque hasta hoy no hacía nada, pero está anotado como cambio
  incompatible en el fragmento de changelog.
- La CLI cambia su texto de ayuda y `config show` distingue las tres situaciones:
  `30 days (set)`, `90 days (default)`, `keeps nothing (set)`. Imprimir un cero
  como "0 days (set)" junto a los demás se leería como un valor todavía no
  alcanzado.
- El mensaje de la API para un negativo pasa a nombrar la salida: "omit it for the
  default, or set it to 0 to keep nothing".
- Es reversible sin migración: es una condición en una función.

## Alternativas descartadas

- **`-1` para "no conservar nada"**: la API rechaza negativos, y admitir uno solo
  como valor mágico obliga a explicar por qué −1 es válido y −2 no.
- **Un flag aparte (`purge: true`)**: dos maneras de expresar la misma ventana, que
  además pueden contradecirse.
- **Dejarlo como estaba y darle a `retention` de la CLI un `-before`**: pone la
  política en un comando en vez de en la configuración del proyecto, y deja el
  ajuste inerte donde estaba.
