-- Undoes 000031_adoption.sql. Adoption jobs, their per-entry results and
-- stored digests are lost. Adopted assets stay, but lose the mark that
-- keeps their objects when they are deleted: delete or detach them first.
DROP TABLE IF EXISTS adoption_entries;
DROP TABLE IF EXISTS adoption_jobs;
DROP TABLE IF EXISTS asset_digests;
DROP INDEX IF EXISTS ux_assets_adopted_key;
ALTER TABLE assets DROP COLUMN IF EXISTS adoption;
