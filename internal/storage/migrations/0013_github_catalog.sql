-- The GitHub catalog row catches up with the provider's grown toolset: the compiled
-- definition now reads CI runs, job logs, file contents and releases beside commits and
-- pull requests, and the seeded reference row must say the same thing.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

UPDATE integration_type
   SET description = 'Read repositories, commits, diffs, pull requests, CI runs and releases from the installations you select: read-only change context.'
 WHERE integration_type_id = 4;
