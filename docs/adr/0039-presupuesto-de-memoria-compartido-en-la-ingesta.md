# 039 — Un presupuesto de memoria compartido para todo lo que la ingesta descomprime a la vez

- **Fecha**: 2026-09-20
- **Estado**: Propuesta — pendiente de ratificación

## Contexto

El endpoint de ingesta es el único público por diseño y su credencial viaja
dentro de bundles de navegador, así que todo lo que llega ahí lo eligió otro.
Los topes por petición estaban puestos y son correctos: 20 MiB por envelope,
4 MiB por item, 100 items, y un `boundedReader` que corta la expansión de gzip
y zstd (`internal/adapters/compression`). Cada uno de esos números responde a
la pregunta *"¿cuánto puede costar una petición?"*.

Ninguno responde a la del atacante, que es *"¿cuánto cuestan mil?"*.

Nada limitaba cuántas peticiones podían sostener esos topes **a la vez**. El
coste real por petición hostil resultó ser la suma de dos cosas que nadie había
sumado: los items que el parser retiene vivos hasta que el handler termina
—hasta 20 MiB— y la ventana del descompresor, que en zstd se dimensiona con lo
que **declara la trama** y que la librería acotaba al máximo tamaño
decodificado, es decir a esos mismos 20 MiB.

Medido en esta rama, con `scripts/hardening.sh` y `compat/load`:

| ataque | subida del atacante | RSS pico del proceso |
|---|---|---|
| 128 bombas gzip a 976:1, concurrencia 32 | 3,2 MB | **649 MB** |
| 128 bombas zstd a 21 346:1, concurrencia 32 | 151 KB | **1,46 GB** |

La segunda fila es una amplificación de ~9 900× sobre el ancho de banda del
atacante y 48× la huella publicada de 30 MB. Y crecía con el ataque: no había
ningún número que publicar, porque el número lo elegía quien atacara.

Es, exactamente, el cuarto caso del mismo error en este repositorio. Argon2id
presupuestado por hash; el encoder de zstd reservando una ventana por núcleo,
medido en una máquina de dos; `/login` con coste por intento y sin techo de
intentos, que llevó la huella a 1,03 GB con 200 logins concurrentes (ADR 023).
Los tres se publicaron como ciertos. La lección registrada es que **un gate que
mide lo que cuesta uno no mide nada**, y aquí faltaba aplicarla al endpoint que
más lo necesita.

Por el camino apareció un segundo defecto, menor en memoria y peor en
comportamiento: el centinela que significa "el cliente envió de más" sólo se
traducía a `ErrTooLarge` en la vía que lee una línea de cabecera, no en la que
lee un payload con longitud declarada — que es la que toman todas las bombas y
todos los SDK reales. El handler respondía **500** a una bomba, la registraba
como `ERROR` y le decía al cliente que reintentara algo que no puede tener
éxito. El log es más barato de llenar que la memoria.

## Decisión

### 1. Un presupuesto en bytes, compartido, que no encola

`internal/adapters/httpapi/ingest_budget.go` añade un `ingestArena`: un
semáforo contador **denominado en bytes**, de **32 MiB**, que vive en
`clientLimits` junto a los limitadores por dirección. Cada petición de ingesta:

1. **Reserva por adelantado** el working set de su descompresor
   (`compression.WorkingSet`: 8 MiB en zstd, 64 KiB en gzip, 0 sin compresión),
   **antes** de construirlo. Después sería tarde: la ventana de zstd ya está
   residente cuando el primer byte es legible.
2. **Cobra a medida que produce**, en trozos de 64 KiB, lo que el parser lee y
   retiene.
3. **Devuelve todo de una vez** al terminar el handler, no al cerrar el body:
   los bytes siguen vivos en el envelope ya parseado.

Si el presupuesto no alcanza, se **rechaza en el acto** con `429` y
`Retry-After: 1`, sin `X-Sentry-Rate-Limits`. No se espera: esperar delante de
un presupuesto de memoria es una forma de retener la memoria igualmente, sólo
que más tarde — el mismo razonamiento por el que el semáforo de autenticación
rechaza en vez de encolar (`throttle.go`).

**Por qué en bytes y no en peticiones.** Un tope por número de peticiones
tendría que asumir que cada una cuesta el máximo, y el máximo es un envelope de
20 MiB: admitiría una o dos a la vez y estrangularía a una instalación que se
comporta bien. Cobrando lo que realmente se produce, un envelope normal de unos
kilobytes paga un trozo de 64 KiB, y caben varios cientos de clientes reales en
el mismo presupuesto que una sola bomba agota.

**Por qué 32 MiB.** El suelo no es opinable: `maxIngestBody` promete que un
envelope de 20 MiB se acepta, y un presupuesto incapaz de admitir uno
convertiría un límite documentado en mentira — un fallo peor que el que se
arregla, porque sólo aparece para quien tiene los eventos más grandes. 32 MiB
son esos 20 MiB más sitio para que el tráfico ordinario siga pasando al lado.

### 2. Un techo a la ventana de zstd

`compression.MaxRequestWindow = 8 MiB`. Sin él, la librería iguala la ventana
máxima al tamaño máximo decodificado, y la petición más barata posible reserva
el buffer más caro posible. Ocho MiB es la ventana más grande que produce
cualquier encoder corriente en sus ajustes por defecto o máximos, así que nada
que envíe un cliente real se rechaza, y una trama que declare más se rechaza
con un error en vez de honrarse.

### 3. El mismo límite, la misma respuesta, caiga donde caiga

`readPayload` traduce `errEnvelopeLimit` a `ErrTooLarge` igual que `readLine`.
Una bomba responde **413** —terminal, que es lo que un SDK no reintenta— y no
escribe una línea de `ERROR` por intento.

### 4. Dos gates que preguntan por mil, no por uno

- `scripts/hardening.sh` manda la misma bomba a dos concurrencias y **afirma
  que el pico no sigue al ataque**: cuadruplicar la avalancha no puede
  cuadruplicar la memoria. Es una afirmación que una sola petición no puede
  hacer ni romper.
- `scripts/footprint.sh` mide cada perfil de tres maneras —en reposo, en pico
  ingiriendo y en pico siendo leído— y lee el pico de `VmHWM`, no de una
  muestra: un pico ya recolectado es invisible para una muestra posterior.

## Consecuencias

- Medido después, con el mismo ataque: **126 MB** (gzip) y **109 MB** (zstd) de
  pico, y **planos** — cuadruplicar la concurrencia mueve la cifra un 10–12 %,
  no un 400 %. La amplificación deja de depender del atacante y pasa a ser
  `presupuesto + holgura del recolector`, que es la única forma en la que una
  cifra de memoria se puede publicar.
- **Un envelope de 20 MiB sigue aceptándose**, y hay un check del gate que lo
  comprueba con uno de 18 MiB, porque es la promesa que este cambio podría
  haber roto en silencio.
- **Dos envelopes máximos simultáneos no caben**: el segundo recibe `429` con
  `Retry-After`. Es deliberado y es visible; un SDK lo trata como fallo
  transitorio y vuelve.
- El presupuesto es una **constante, no un flag**. Un knob aquí es un knob que
  alguien acaba subiendo para que desaparezca un error, que es precisamente
  cómo se llega a la huella que este ADR arregla. Si resulta estrecho para
  alguna instalación real, se cambia la constante con la medición delante.
- Un despliegue detrás de un proxy que ya encola peticiones verá el `429` antes
  de lo que lo vería sin presupuesto; a cambio, no verá al proceso morir por
  OOM, que es el fallo que sustituye.
- `internal/domain/cron.go` gana `cronBit`, una función que hace el
  desplazamiento con su cota comprobada en un sitio en vez de seis. Es
  consecuencia del gate de `gosec`, no de este diseño, y está aquí porque la
  cota que ahora se comprueba no se comprobaba antes.
