-- Agent reads of KB docs (#159): when an agent last read a doc, who it was,
-- and how many reads have been counted, so a person (or a KB-cleanup agent)
-- can find docs nobody reads.
--
-- A read is counted when an MCP amp_kb_get returns the doc or amp_kb_search
-- returns one of its chunks. Writes are throttled per (doc, reader) in
-- kb_doc_reads: the same reader reading the same doc again within the
-- throttle window (internal/kb.ReadThrottle) changes nothing, even if other
-- readers read it in between, so a busy doc costs at most one row write per
-- reader per window and read_count counts those windows, not every call.
-- last_read_at/by are the last counted read. People reading in the Library
-- are not counted.
--
-- Additive only: existing rows start never-read (NULL, NULL, 0).

ALTER TABLE kb_documents ADD COLUMN IF NOT EXISTS last_read_at TIMESTAMPTZ;
ALTER TABLE kb_documents ADD COLUMN IF NOT EXISTS last_read_by TEXT;
ALTER TABLE kb_documents ADD COLUMN IF NOT EXISTS read_count BIGINT NOT NULL DEFAULT 0;

-- One row per (doc, reader): when that reader's read of the doc was last
-- counted. Readers are agents, so this stays small.
CREATE TABLE IF NOT EXISTS kb_doc_reads (
    document_id BIGINT NOT NULL REFERENCES kb_documents(id) ON DELETE CASCADE,
    reader      TEXT NOT NULL,
    last_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (document_id, reader)
);
