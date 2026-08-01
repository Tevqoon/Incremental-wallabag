-- Lets a queued write be traced back to the local element it came from.
--
-- Every write so far has been "set a flag" or "delete this" — nothing about
-- them needed the local row again once they landed. Pushing a brand-new
-- highlight is different: wallabag hands back an id for the annotation it
-- just created, and that id has to be written onto the extract that caused
-- it, or a later delete of that same extract would have no id to remove
-- upstream. NULL for every existing write kind, which never needs it.
ALTER TABLE pending_writes ADD COLUMN element_id INTEGER REFERENCES elements(id) ON DELETE CASCADE;
