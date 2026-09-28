# Fragmentos de changelog

Cada cambio deja aquí **su propio fichero**, `<id>.md`, con lo que haya que
contarle a quien use el producto. Nadie edita `CHANGELOG.md` directamente
hasta que se corta una release.

Existe para que dos ramas que tocan el changelog a la vez no tengan que
fusionar prosa a mano en cada rebase: un fichero por cambio no colisiona nunca.

```md
### Añadido
- Lo que ahora se puede hacer y antes no.

### Corregido
- El síntoma primero, en negrita si el bug importaba, y después la causa.

### Cambiado
- Lo que se comporta distinto, y qué tiene que hacer quien ya lo usaba.
```

Reglas:

- El fichero empieza por una cabecera `###`.
- Se escribe para quien **usa** el producto, no para quien lo desarrolla: el
  porqué de una decisión va en un ADR, no aquí.
- El orden de montaje es el de los nombres ordenados, así que el id manda:
  una fecha o un número de secuencia delante (`2026-10-02-retention.md`) deja
  la historia en orden.

`make changelog` lo muestra montado sin tocar nada;
`make changelog VERSION=vX.Y.Z` lo pega en `CHANGELOG.md` y borra los
fragmentos consumidos.
