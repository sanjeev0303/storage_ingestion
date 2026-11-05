-- An adoption job reserves the organization's quota for every object it will
-- adopt before it adopts any, as an upload does for its bytes. Each adopted
-- object turns its share into stored usage; the rest is given back when the
-- job ends.
ALTER TABLE adoption_jobs ADD COLUMN IF NOT EXISTS reserved_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE adoption_jobs ADD COLUMN IF NOT EXISTS reserved_assets BIGINT NOT NULL DEFAULT 0;
