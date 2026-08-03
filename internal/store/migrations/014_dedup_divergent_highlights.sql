-- The two duplicate groups 013 deliberately left alone, resolved by hand
-- after asking: in both, the surviving copy is the one that was actually
-- read or graded, and the other — untouched since the 2026-08-01 batch
-- import created it — is discarded.
--
-- Local only, same as 013: this does not touch either row's external_ref
-- or queue anything upstream. Which of the two wallabag-side annotation ids
-- is still live is not known with confidence, and deleting the wrong one
-- would be worse than leaving both alone upstream.
--
-- "How I Biohack Intelligence - Desmolysium": keep 1197 (state=done),
-- drop 1196 (state=new, untouched).
DELETE FROM elements WHERE id = 1196;

-- "Reading is magic": keep 1228 (state=reading, one rep, rescheduled),
-- drop 1227 (state=new, untouched).
DELETE FROM elements WHERE id = 1227;
