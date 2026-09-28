-- A name a deploy tool can put in a URL.
--
-- sentry-cli addresses a project by slug: SENTRY_PROJECT goes straight into
-- the path of every call it makes (ADR 013). The numeric id is accepted too
-- and always will be — it is what the ingest path carries — but a pipeline
-- configuration that reads `SENTRY_PROJECT=venekambio` is the one people copy
-- from a documentation page, and refusing it would make the compatibility
-- claim true only for someone who first looked up an integer.
--
-- The column is on projects rather than in a table of its own because it is an
-- attribute of the project, unique across the installation, and there is one
-- of them. A join to find a name is a join nobody should pay.
ALTER TABLE projects ADD COLUMN slug TEXT NOT NULL DEFAULT '';

-- Backfill. Every existing project gets a slug derived from its name, by the
-- same rules internal/domain/project.go applies to a new one: fold the Latin
-- accents, lowercase, turn every run of anything else into a single hyphen,
-- trim the hyphens off the ends.
--
-- Written as successive statements rather than one nest of replace() calls
-- thirty deep. It runs once, over a table with as many rows as an installation
-- has applications, and a backfill nobody can read is a backfill nobody can
-- check.
UPDATE projects SET slug = name;

-- Accents first: SQLite's lower() is ASCII-only, so 'Á' would survive it and
-- then be destroyed by the sweep below. Folding is deliberate — 'Diseño'
-- should read as 'diseno', not as 'dise-o'.
UPDATE projects SET slug = replace(slug, 'Á', 'a');
UPDATE projects SET slug = replace(slug, 'É', 'e');
UPDATE projects SET slug = replace(slug, 'Í', 'i');
UPDATE projects SET slug = replace(slug, 'Ó', 'o');
UPDATE projects SET slug = replace(slug, 'Ú', 'u');
UPDATE projects SET slug = replace(slug, 'Ü', 'u');
UPDATE projects SET slug = replace(slug, 'Ñ', 'n');
UPDATE projects SET slug = replace(slug, 'Ç', 'c');
UPDATE projects SET slug = replace(slug, 'á', 'a');
UPDATE projects SET slug = replace(slug, 'é', 'e');
UPDATE projects SET slug = replace(slug, 'í', 'i');
UPDATE projects SET slug = replace(slug, 'ó', 'o');
UPDATE projects SET slug = replace(slug, 'ú', 'u');
UPDATE projects SET slug = replace(slug, 'ü', 'u');
UPDATE projects SET slug = replace(slug, 'ñ', 'n');
UPDATE projects SET slug = replace(slug, 'ç', 'c');

UPDATE projects SET slug = lower(slug);

-- The separators a project name actually contains.
UPDATE projects SET slug = replace(slug, ' ', '-');
UPDATE projects SET slug = replace(slug, char(9), '-');
UPDATE projects SET slug = replace(slug, char(10), '-');
UPDATE projects SET slug = replace(slug, '_', '-');
UPDATE projects SET slug = replace(slug, '.', '-');
UPDATE projects SET slug = replace(slug, '/', '-');
UPDATE projects SET slug = replace(slug, '\', '-');
UPDATE projects SET slug = replace(slug, ':', '-');
UPDATE projects SET slug = replace(slug, '@', '-');
UPDATE projects SET slug = replace(slug, '+', '-');
UPDATE projects SET slug = replace(slug, '&', '-');
UPDATE projects SET slug = replace(slug, ',', '-');
UPDATE projects SET slug = replace(slug, '''', '-');
UPDATE projects SET slug = replace(slug, '"', '-');
UPDATE projects SET slug = replace(slug, '(', '-');
UPDATE projects SET slug = replace(slug, ')', '-');
UPDATE projects SET slug = replace(slug, '[', '-');
UPDATE projects SET slug = replace(slug, ']', '-');
UPDATE projects SET slug = replace(slug, '!', '-');
UPDATE projects SET slug = replace(slug, '?', '-');
UPDATE projects SET slug = replace(slug, '#', '-');
UPDATE projects SET slug = replace(slug, '%', '-');
UPDATE projects SET slug = replace(slug, '*', '-');

-- Collapse runs. Four passes halve a run each time, so this flattens any run
-- up to sixteen hyphens — longer than any project name that is mostly
-- punctuation, and such a name falls out at the validity sweep below anyway.
UPDATE projects SET slug = replace(slug, '--', '-');
UPDATE projects SET slug = replace(slug, '--', '-');
UPDATE projects SET slug = replace(slug, '--', '-');
UPDATE projects SET slug = replace(slug, '--', '-');

UPDATE projects SET slug = ltrim(rtrim(slug, '-'), '-');
UPDATE projects SET slug = substr(slug, 1, 50);
UPDATE projects SET slug = rtrim(slug, '-');

-- Three sweeps hand a row back to the fallback, and between them they are what
-- makes the unique index below impossible to violate.
--
-- 1. Anything the rules above could not reduce to [a-z0-9-]: a name in a
--    script this derivation does not know.
-- 2. Anything that is only digits, which would be shadowed by the id: the
--    compatibility surface accepts a numeric segment as an id first, so a slug
--    that looks like one could never be resolved as a slug.
-- 3. Anything starting with the fallback's own prefix, which is what reserves
--    'project-<id>' — an id is unique, so once no derived slug can begin with
--    'project-', the fallback cannot collide with anything.
UPDATE projects SET slug = '' WHERE slug GLOB '*[^a-z0-9-]*';
UPDATE projects SET slug = '' WHERE slug <> '' AND slug NOT GLOB '*[a-z]*';
UPDATE projects SET slug = '' WHERE slug LIKE 'project-%';

-- Two projects whose names derive the same slug: the older one keeps it.
UPDATE projects SET slug = ''
 WHERE slug <> ''
   AND id <> (SELECT MIN(other.id) FROM projects other WHERE other.slug = projects.slug);

UPDATE projects SET slug = 'project-' || id WHERE slug = '';

-- Created last on purpose: the backfill above blanks rows on its way through,
-- and a unique index in place would reject the second blank.
CREATE UNIQUE INDEX idx_projects_slug ON projects (slug);
