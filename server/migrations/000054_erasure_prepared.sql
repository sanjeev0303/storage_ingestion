-- An erasure is prepared once its successor snapshots are made and the
-- drafts emptied (docs/erasure.md). Until then the cleanup worker makes
-- them again before it asks for any deletion, and never certifies it.
ALTER TABLE erasure_requests ADD COLUMN IF NOT EXISTS prepared_at TIMESTAMPTZ;
