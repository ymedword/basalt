-- A reindex needs an independent immutable version even when its content is unchanged.
-- Assign every version a monotonic revision; the document lock serializes new attempts.
ALTER TABLE kb_document_versions
    DROP CONSTRAINT kb_document_versions_document_id_content_hash_key;

CREATE SEQUENCE kb_document_version_revision_seq;
ALTER TABLE kb_document_versions ADD COLUMN revision bigint;
WITH ordered AS (
    SELECT version_id, row_number() OVER (ORDER BY created_at, version_id) AS revision
    FROM kb_document_versions
)
UPDATE kb_document_versions AS v SET revision = ordered.revision
FROM ordered WHERE v.version_id = ordered.version_id;
SELECT setval('kb_document_version_revision_seq',
              COALESCE((SELECT MAX(revision) FROM kb_document_versions), 1),
              EXISTS (SELECT 1 FROM kb_document_versions));
ALTER TABLE kb_document_versions
    ALTER COLUMN revision SET DEFAULT nextval('kb_document_version_revision_seq'),
    ALTER COLUMN revision SET NOT NULL;
CREATE UNIQUE INDEX ux_kb_document_versions_revision ON kb_document_versions(revision);
CREATE INDEX idx_kb_document_versions_doc_hash_revision
    ON kb_document_versions(document_id, content_hash, revision DESC);
