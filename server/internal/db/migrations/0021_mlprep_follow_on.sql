-- +goose Up
-- One bit that says why a job was queued, because two callers want different
-- things from the same kind.
--
-- An mlprep job does two things when it finishes: it writes the renditions, and
-- it queues the three passes that read them. That pairing is correct for the
-- reason runMLPrep gives — a photograph arriving from a phone and being
-- described a minute later is the feature, and nobody is going to type a command
-- per upload.
--
-- It is wrong for `photobackup ml renditions`. That command re-renders the whole
-- archive after a change to derivstore.MLEdge, and it says in its own output
-- that nothing already embedded, read or captioned is requeued by it. True, as
-- far as it went: Enqueue is on-conflict-do-nothing, so it never touched a row
-- that existed. What it did instead was create the rows that did not — a first
-- describe job for every asset the bounded captioning backfill had never
-- reached, which on this archive was most of it. Seventeen thousand of them,
-- arriving at the rate the CPU could render, interleaved with the ocr jobs
-- landing beside them.
--
-- That interleaving is what broke: jobs.ClaimInOrder guarantees ocr drains
-- before describe within a fixed queue, and app.py's one-directional captioner
-- eviction is built on that guarantee. A queue being refilled from underneath
-- for hours is not a fixed queue. The captioner and the recogniser met, the card
-- had no room for both, and the pass spent two hours writing down that it had
-- looked at photographs it had never loaded.
--
-- So the intent travels with the job rather than being inferred at the far end.
-- Default true because arrival is the common case and every existing row is one.
alter table jobs add column follow_on boolean not null default true;

comment on column jobs.follow_on is
    'Whether finishing this job queues the work that depends on it. False is a bulk '
    're-render asking for its own output and nothing further; see jobs.RequeueMLPrep.';

-- +goose Down
alter table jobs drop column follow_on;
