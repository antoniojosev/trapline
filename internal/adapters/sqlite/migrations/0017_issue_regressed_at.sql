-- When an issue last came back.
--
-- `regressions` counts how many times an issue has reopened and
-- `regressed_in_release` names the build that brought it back (migration
-- 0009), but nothing records *when*. That gap is invisible on an issue page —
-- which only ever asks "has this come back", a question the counter answers —
-- and fatal to anything that has to answer it about a period.
--
-- The weekly digest is exactly that: "what came back this week" is one of the
-- three lines it exists to write. Without this column the honest answers all
-- require reading events (the scan ADR 001 forbids, and one that stops working
-- the day retention deletes the payloads) and the cheap answers are wrong:
-- "unresolved with regressions > 0 and seen this week" reports an issue that
-- came back in March and has been noisy ever since as this week's news. A
-- weekly mail that cries regression at old news is a weekly mail people stop
-- opening, which is the only way this feature can actually fail.
--
-- NULL means "has never come back", which is the overwhelming majority of
-- rows and what every issue that existed before this migration says.
ALTER TABLE issues ADD COLUMN regressed_at TEXT;

-- "What came back in this window, in this project" is a range scan over this
-- index. Without it the digest reads every issue of the project once a week
-- to find the two that matter.
--
-- Rows where the column is NULL are not in the index: SQLite omits them, so
-- an installation where nothing has ever regressed pays nothing for it.
CREATE INDEX idx_issues_project_regressed_at
    ON issues (project_id, regressed_at)
    WHERE regressed_at IS NOT NULL;
