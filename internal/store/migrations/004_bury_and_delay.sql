-- "Not this one right now, but before I stop reading today."
--
-- A date rather than a flag, so it clears itself: tomorrow the value no longer
-- matches today and the element sorts normally again, with nothing to reset and
-- no way to leave something buried by accident.
ALTER TABLE elements ADD COLUMN buried_on TEXT;

-- Spread the imported backlog across the delay window.
--
-- Every highlight imported so far became due on the day it arrived, so a
-- library's worth of them lands at once — 459 in the case this was written for,
-- against 36 unread articles. Simply pushing them all out by the delay would
-- move the pile rather than remove it, so they are scattered across the window
-- instead.
--
-- The multiplier is the same one the queue's tie-break uses. Reusing it means
-- the two orderings agree rather than interfering: elements adjacent in the
-- queue hash are also adjacent in due date, instead of one scrambling what the
-- other just arranged.
--
-- Offsets run 1..10, never 0: nothing should stay due the day it was imported,
-- which is the whole point of moving it.
--
-- Only untouched imports are moved. Anything already graded has a due date the
-- reader chose, and manual extracts were created deliberately in a session that
-- expected them back.
UPDATE elements
SET due_on = date('now', '+' || (((id * 2654435761) % 10) + 1) || ' days')
WHERE parent_id IS NOT NULL
  AND origin = 'import'
  AND state = 'new'
  AND reps = 0
  AND due_on <= date('now');
