-- 012_mnemes_content.sql
--
-- Consolidation phase: give semantic.mnemes a content-bearing surface so
-- the semantic recall tier can contribute readable text (it carried only
-- an opaque embedding before — LME bench showed content-less mnemes were
-- useless to RRF). All columns are additive + nullable so this is a
-- zero-downtime migration over the existing 439 sidecar-era rows.
--
-- Excerpts in `representatives` are deliberate COPIES of source-event
-- text, not pointers: a mneme must survive future eviction of its source
-- events (CLS — the distilled trace outlives the raw one). This is the
-- storage-economics foundation the roadmap builds on.

ALTER TABLE semantic.mnemes
    ADD COLUMN IF NOT EXISTS label           text,
    ADD COLUMN IF NOT EXISTS representatives jsonb,
    ADD COLUMN IF NOT EXISTS tags            text[],
    ADD COLUMN IF NOT EXISTS entities        text[],
    ADD COLUMN IF NOT EXISTS span_start      timestamptz,
    ADD COLUMN IF NOT EXISTS span_end        timestamptz,
    ADD COLUMN IF NOT EXISTS meta            jsonb;

-- GIN indexes for tag/entity overlap queries (future recall filters +
-- HOLA analysis). Partial-free: tags/entities are small arrays, the GIN
-- index is cheap and the columns are queried across all states.
CREATE INDEX IF NOT EXISTS mnemes_tags_gin
    ON semantic.mnemes USING gin (tags);
CREATE INDEX IF NOT EXISTS mnemes_entities_gin
    ON semantic.mnemes USING gin (entities);
