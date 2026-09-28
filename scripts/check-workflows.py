#!/usr/bin/env python3
"""Comprueba que cada workflow declara jobs que GitHub puede ejecutar.

Existe porque un job se quedó sin `runs-on` ni `steps` al resolver un conflicto
de rebase, y Actions rechaza el fichero **entero** cuando un solo job es
inválido: durante varios merges no se ejecutó ni un gate en CI, sin que nada
lo dijera. El YAML seguía siendo YAML válido, así que ningún linter protestó.

La segunda comprobación —claves duplicadas— se añadió después por el mismo
motivo y tras el mismo accidente: otro conflicto dejó **dos** jobs `releases`
en `ci.yml`, y el primero corría `stats.sh`. `yaml.safe_load` se queda con el
último y no dice nada, así que este script informaba de 14 jobs correctos
mientras Actions rechazaba el fichero entero por la clave repetida. Un
verificador que normaliza lo que verifica no verifica nada.
"""
import re
import sys, yaml, pathlib


class StrictLoader(yaml.SafeLoader):
    """SafeLoader que se niega a quedarse con la última de dos claves iguales."""


def _no_duplicates(loader, node, deep=False):
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in mapping:
            raise yaml.constructor.ConstructorError(
                None, None,
                f"clave duplicada {key!r} en la línea {key_node.start_mark.line + 1}; "
                "Actions rechaza el fichero entero",
                key_node.start_mark)
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


StrictLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _no_duplicates)

REQUIRED = ("runs-on", "steps")
JOB_KEY = re.compile(r"^  ([A-Za-z0-9_-]+):\s*$")


def duplicate_job_keys(text):
    """Nombres de job declarados más de una vez.

    Hay que buscarlos en el texto, no en el YAML ya cargado: PyYAML se queda
    con la última de dos claves iguales y no dice nada, mientras que Actions
    rechaza el fichero entero. Un comprobador que sólo mire el árbol cargado
    da luz verde justo al fallo que se supone que vigila — pasó, y por eso
    esta función existe.
    """
    seen, dupes = set(), []
    in_jobs = False
    for line in text.splitlines():
        if line.startswith("jobs:"):
            in_jobs = True
            continue
        stripped = line.strip()
        # Un comentario o una línea en blanco no cierran el bloque `jobs:`,
        # y este fichero termina en un comentario a nivel cero: tratarlos
        # como cierre apagaba la detección justo antes del final.
        if not stripped or stripped.startswith("#"):
            continue
        if in_jobs and not line.startswith((" ", "\t")):
            in_jobs = False
        if not in_jobs:
            continue
        m = JOB_KEY.match(line)
        if m:
            name = m.group(1)
            if name in seen:
                dupes.append(name)
            seen.add(name)
    return dupes
bad = []
for path in sorted(pathlib.Path(".github/workflows").glob("*.yml")):
    text = path.read_text(encoding="utf-8")
    for name in duplicate_job_keys(text):
        bad.append(f"{path}: el job {name} está declarado dos veces")
    try:
        doc = yaml.load(text, Loader=StrictLoader)
    except yaml.YAMLError as err:
        bad.append(f"{path}: {err}")
        print(f"{path}: ILEGIBLE")
        continue
    jobs = (doc or {}).get("jobs") or {}
    if not jobs:
        bad.append(f"{path}: sin jobs")
        continue
    for name, job in jobs.items():
        if not isinstance(job, dict):
            bad.append(f"{path}: el job {name} no es un mapa")
            continue
        missing = [k for k in REQUIRED if k not in job and "uses" not in job]
        if missing:
            bad.append(f"{path}: el job {name} no tiene {', '.join(missing)}")
    print(f"{path}: {len(jobs)} jobs")

if bad:
    print("\n".join("  " + b for b in bad), file=sys.stderr)
    sys.exit(1)
print("todos los jobs están completos")
