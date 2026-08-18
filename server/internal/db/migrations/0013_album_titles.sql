-- +goose Up
-- Albums stop being something only an import can make.
--
-- Nothing about the table changes: a title, a description and a membership list
-- were always enough, and the columns an import fills are the same ones a
-- person filling in a dialog fills. What changes is who is allowed to write
-- them, and that turns one existing constraint into a problem.
--
-- `unique (source, title)` was written for the import, where it is exactly
-- right: an export has no album ids, a directory name is the whole identity,
-- and the constraint is what makes running the same import twice produce one
-- album rather than two. But it counts rows the gallery cannot see. An album
-- deleted by hand keeps its row for a year — see migration 0011 — and until
-- this it kept its *name* for a year too, so deleting "Iceland" and making a
-- new "Iceland" the same afternoon failed against a row in Recently Deleted
-- that nobody was looking at.
--
-- Scoped to the live rows, which is the set the name is actually a name in.
-- Deleting an album now releases its title, and restoring one can therefore
-- collide — which is a real edge and the right one to have: the restore is an
-- Undo clicked seconds later, and the alternative was a name held hostage by
-- something already thrown away.
--
-- Deliberately *not* scoped by `vault`. A hidden album's title still occupies
-- the name, so making a library album called what some archived album is called
-- is refused. That is one bit of leakage — somebody can learn that *a* hidden
-- album has this title by being told the name is taken — and it buys the thing
-- that matters more: hiding an album is an UPDATE of the `vault` column alone,
-- so it can never fail on a uniqueness check, and neither can taking one back
-- out. A predicate this index could be pushed into or out of by an update is a
-- predicate that turns Archive into an operation with a failure mode.
alter table albums drop constraint albums_source_title_key;

create unique index albums_source_title_key on albums (source, title)
    where deleted_at is null;

-- +goose Down
drop index albums_source_title_key;

-- Restoring the total constraint can fail where the partial one was doing its
-- job: two live-and-deleted albums may now share a name. That is inherent to
-- going back and is better as a loud error than as a silent merge.
alter table albums add constraint albums_source_title_key unique (source, title);
