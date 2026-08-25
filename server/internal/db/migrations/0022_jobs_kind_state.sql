-- +goose Up
-- The index behind jobs.Quiet, which asks twice per poll whether the cheap ML
-- passes have gone quiet: nothing of theirs pending or running, and nothing of
-- theirs finished in the last two minutes.
--
-- Without it both halves are a sequential scan — 3.4ms over 52,000 rows today,
-- four workers asking every five seconds, and growing with the archive rather
-- than with the queue. The existing jobs_claim_idx cannot answer either half:
-- it is partial on state = 'pending' and carries run_after, not updated_at.
--
-- Not partial, because the two halves want different states and a single index
-- over (kind, state, updated_at) serves both — an equality probe for the first,
-- and a range on the third column for the second.
create index jobs_kind_state_updated_idx on jobs (kind, state, updated_at);

-- +goose Down
drop index jobs_kind_state_updated_idx;
