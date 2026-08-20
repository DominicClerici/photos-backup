-- +goose Up
-- Two things a joined recording can be that `state` has no room for.
--
-- The segments half of the merge feature resolves itself: a worker finds six
-- ten-second pieces, concatenates them, archives the result and trashes the
-- pieces, and the review page is a log of what it did rather than a queue of
-- questions. That log is the problem this migration is about. It only ever
-- grows, every row on it has already been dealt with, and there is no way to
-- say "I have looked at this one" — so the page and the status card that counts
-- it both go on asking for attention that was paid weeks ago.
--
-- The other gap is the opposite: a join that did not happen at all. The job
-- fails, the group stays pending, and the only trace is a row in the status
-- page's failure list — on a page that cannot do anything about it. The join
-- itself is usually fine; what failed is the check that the running time adds
-- up, which is deliberately unforgiving (see video.joinDurationSlack) because
-- the failure it is really guarding against is ffmpeg silently dropping a part.
-- Somebody who has watched the result and can see that nothing is missing needs
-- a way to say so.

-- When somebody last looked at this join and was content with it.
--
-- Not a state. The three states are what happened to the group — proposed,
-- resolved, refused — and every one of them is a fact about the photographs. An
-- approval is a fact about the person: it says the log entry has been read, and
-- it changes nothing about the assets, which is exactly why undoing it has no
-- consequences and why "split back up" is still available afterwards.
--
-- Null on every existing row, which is the honest answer: nothing has been
-- reviewed, because until now there was nothing to review with.
alter table merge_groups add column approved_at timestamptz;

-- Join these parts even though their durations do not add up.
--
-- Set by hand, from the review page, and only after the rejected join has been
-- watched — the merge worker keeps the file it refused to archive precisely so
-- that the decision is made by looking rather than by arguing with a constant.
-- It survives on the group rather than being a one-shot flag on the job because
-- the job is retried, requeued and reclaimed by machinery that knows nothing
-- about this, and every one of those attempts has to make the same choice the
-- first one did.
alter table merge_groups add column force_join boolean not null default false;

-- The joined-recordings page's two queries, which are both narrow and both run
-- against a table that is mostly resolved duplicate groups.
create index merge_groups_segments_idx on merge_groups (state, detected_at)
    where kind = 'video-segments';

-- +goose Down
drop index merge_groups_segments_idx;
alter table merge_groups drop column force_join;
alter table merge_groups drop column approved_at;
