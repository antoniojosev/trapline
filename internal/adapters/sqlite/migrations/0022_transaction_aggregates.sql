-- What every transaction cost, in windows, with a mergeable sketch per window.
--
-- The columns are three composable numbers and one sketch, and the split is
-- the whole of ADR 007. `count` and `failed` add up across windows; a
-- percentile does not. Averaging the p95 of sixty minutes does not give the
-- p95 of the hour, and with uneven traffic it is not even close — a minute
-- holding three slow requests would weigh the same as a minute holding ten
-- thousand fast ones. So the percentile is never stored: what is stored is a
-- logarithmic histogram (internal/engine/sketch, ADR 020), and the percentile
-- is computed when somebody asks by merging the sketches of the range.
--
-- Two tables at two granularities, and the pair is what makes the feature
-- affordable. Minutes answer "the deploy went out at 14:32 and the p95
-- tripled", which an hourly bucket averages away, and they are kept for 48
-- hours because that is how long anybody asks a minute-level question. Hours
-- answer everything else and are kept for the configured transaction window
-- (7 days by default), at a sixtieth of the rows. The `downsample` job folds
-- closed minutes into their hour and deletes them — and the fold is exact,
-- because merging two sketches with the same bucket layout is adding counts
-- (ADR 021).
--
-- `sampled` is the one column that is not about latency: how many of the
-- transactions counted here also had their raw spans stored. The aggregates
-- are computed over 100% of what arrives and the raw traces over a fraction of
-- it, so without this a panel could not tell a reader which of the two numbers
-- they are looking at — and "we only kept a tenth of them" is exactly the kind
-- of thing somebody should not have to discover from a support thread.

CREATE TABLE txn_minute (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    -- The aggregation key: the route, the job, the command. Bounded by the
    -- domain at 200 characters, because an SDK that names transactions after
    -- URLs with ids in them is the classic way to turn an aggregate into a
    -- copy of the event log.
    --
    -- `txn_name` and not `transaction`, which is what the plan called it and
    -- what the API still calls it. TRANSACTION is a reserved word in SQLite,
    -- so the column would have to be double-quoted in every statement that
    -- names it — and a double-quoted identifier SQLite does not recognise is
    -- silently a string literal, so one typo would make every row report the
    -- same made-up name instead of failing. A column name nobody has to quote
    -- removes the possibility rather than relying on nobody making the typo.
    txn_name    TEXT    NOT NULL,
    -- 'YYYY-MM-DDTHH:MM' in UTC. Text and fixed width, like every other
    -- instant in this database, so it sorts lexicographically in the order it
    -- sorts chronologically and a range is an index scan (ADR 033).
    minute      TEXT    NOT NULL,
    count       INTEGER NOT NULL DEFAULT 0,
    -- How many of them had a status outside {ok, cancelled, unknown}. A
    -- counter and not a rate, for the same reason latency is a sketch: a rate
    -- cannot be merged with another window's and a count can.
    failed      INTEGER NOT NULL DEFAULT 0,
    -- How many had their raw spans kept, so the effective sampling rate is a
    -- fact a reader can see rather than a setting they have to go and look up.
    sampled     INTEGER NOT NULL DEFAULT 0,
    -- The latency histogram, in the encoding internal/engine/sketch writes:
    -- a version byte, the exact count/sum/min/max, and delta-coded bucket
    -- pairs. Read, merged and written back inside the ingest transaction —
    -- SQLite has one writer, which is what makes read-modify-write safe here
    -- without a lock of our own.
    sketch      BLOB    NOT NULL,

    PRIMARY KEY (project_id, txn_name, minute)
) STRICT;

-- The listing's query: every transaction of one project over a range, ranked.
-- The primary key fixes the name before the minute, so "this project between
-- these two minutes, across all names" cannot use it.
CREATE INDEX idx_txn_minute_range ON txn_minute (project_id, minute);

CREATE TABLE txn_hour (
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    txn_name    TEXT    NOT NULL,
    -- 'YYYY-MM-DDTHH', the same layout the error aggregates use (ADR 010), so
    -- a reader comparing an error chart with a latency chart is comparing two
    -- columns written the same way.
    hour        TEXT    NOT NULL,
    count       INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    sampled     INTEGER NOT NULL DEFAULT 0,
    sketch      BLOB    NOT NULL,

    PRIMARY KEY (project_id, txn_name, hour)
) STRICT;

CREATE INDEX idx_txn_hour_range ON txn_hour (project_id, hour);
