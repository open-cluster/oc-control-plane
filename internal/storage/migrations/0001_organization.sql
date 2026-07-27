-- The organization directory: the tenant boundary and the durable home of its placement
-- assignment. Where an organization's data lives is resolved from the organization and is
-- never ambient.
--
-- Assignments are read from process configuration today; this table is the durable form
-- that resolution moves to once organizations become creatable. The column is therefore
-- present and authoritative from the first migration, so the later move is a change of
-- source rather than a schema change under load.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

CREATE TABLE IF NOT EXISTS organization
(
    -- The external identity provider's organization identifier, bounded to match
    -- tenancy.Organization's own limit so the two cannot disagree.
    id         TEXT        NOT NULL PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 128),

    -- The placement whose database holds this organization's data. Names a placement
    -- defined in process configuration; an unresolvable value is a startup failure, never
    -- a silent fallback to a shared connection.
    placement  TEXT        NOT NULL CHECK (length(placement) BETWEEN 1 AND 64),

    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE organization IS
    'Tenant boundary. Every durable record belongs to exactly one organization.';
COMMENT ON COLUMN organization.placement IS
    'Placement name; resolves to a database, and later to object storage, region, and model provider.';
