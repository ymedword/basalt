-- A collection's purpose is user-managed metadata, separate from its documents.
-- The revision provides optimistic concurrency for edits by multiple administrators.
ALTER TABLE kb_collections ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE kb_collections ADD COLUMN description_revision bigint NOT NULL DEFAULT 0;
