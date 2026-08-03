-- A bulk resync on 2026-08-01 re-imported a batch of already-imported
-- highlights under new external_refs, producing exact duplicate elements
-- rows (same document, same quote) instead of updating the existing one.
-- Confirmed one-time: every duplicate row across every affected document
-- shares that exact same created_at timestamp, and nothing since has
-- produced another one.
--
-- This removes the redundant copy, keeping the lowest id (the original,
-- pre-batch row) per (document_id, quote) group. It only touches groups
-- where every duplicate still has identical state, reps and due_on — i.e.
-- neither copy has been read, graded or rescheduled independently of the
-- other since the batch created it. A couple of groups did diverge (one
-- copy graded or marked done while its twin sat untouched); those are left
-- alone here, since picking a side would silently discard real reading
-- progress that only a person can judge.
DELETE FROM elements
WHERE origin = 'import'
  AND id NOT IN (
    SELECT MIN(id) FROM elements
    WHERE origin = 'import'
    GROUP BY document_id, quote
  )
  AND (document_id, quote) IN (
    SELECT document_id, quote FROM elements
    WHERE origin = 'import'
    GROUP BY document_id, quote
    HAVING COUNT(*) > 1
       AND MIN(state) = MAX(state)
       AND MIN(reps) = MAX(reps)
       AND MIN(IFNULL(due_on, '')) = MAX(IFNULL(due_on, ''))
  );
