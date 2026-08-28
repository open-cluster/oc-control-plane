CREATE TABLE organization (
    org_id text PRIMARY KEY,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT organization_org_id_check CHECK (length(org_id) BETWEEN 1 AND 128),
    CONSTRAINT organization_created_by_check CHECK (length(created_by) BETWEEN 1 AND 256)
);

INSERT INTO organization (org_id, created_by)
SELECT DISTINCT org_id, 'schema migration'
  FROM organization_membership
ON CONFLICT (org_id) DO NOTHING;
