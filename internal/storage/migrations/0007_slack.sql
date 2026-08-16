-- Slack joins the catalog, and the integration row learns to hold a sealed outbound
-- credential.
--
-- The credential columns arrive here, with their first consumer, rather than in the
-- baseline: Alertmanager and Kubernetes present nothing outbound, so until now there was
-- nothing for the columns to hold. Later credential-bearing providers share them.
--
-- Migrations are forward-only and append-only. An applied migration is never edited.

INSERT INTO integration_type (integration_type_id, key, name, description, logo, category)
VALUES (3, 'slack', 'Slack',
        'Read incident conversation from your workspace''s channels: bounded, read-only, never posting.',
        'slack', 'collaboration')
ON CONFLICT (integration_type_id) DO NOTHING;

-- The sealed outbound credential: AES-256-GCM under the deployment's sealing key, because
-- this is the one kind of credential that must be PRESENTED to the provider rather than
-- compared against. NOTHING HERE IS EVER RETURNED BY ANY API; reads render the metadata
-- columns only.
ALTER TABLE integration
    ADD COLUMN IF NOT EXISTS credential_sealed BYTEA,

    -- A MINTED identity for the live credential, never derived from it, for the same
    -- reason the webhook fingerprint is minted: a derived value would let a database dump
    -- confirm a guess offline. It is what lets an operator tell one pasted token from the
    -- next after a replacement.
    ADD COLUMN IF NOT EXISTS credential_fingerprint TEXT CHECK (
        credential_fingerprint IS NULL OR length(credential_fingerprint) BETWEEN 1 AND 64),
    ADD COLUMN IF NOT EXISTS credential_created_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS credential_rotated_at TIMESTAMPTZ;

-- The credential travels whole or not at all, mirroring the webhook secret's constraint:
-- sealed bytes nobody can identify, or an identity with nothing behind it, are both states
-- an operator cannot act on.
ALTER TABLE integration
    ADD CONSTRAINT integration_credential_is_whole CHECK (
        (credential_sealed IS NULL AND credential_fingerprint IS NULL
            AND credential_created_at IS NULL)
            OR (credential_sealed IS NOT NULL AND credential_fingerprint IS NOT NULL
            AND credential_created_at IS NOT NULL)
        );

COMMENT ON COLUMN integration.credential_sealed IS
    'Outbound credential, AES-256-GCM sealed under the deployment sealing key. Write-only after entry; no API returns it.';
COMMENT ON COLUMN integration.credential_fingerprint IS
    'A minted identity for the live credential. Not derived from it: a derived value would let a dump confirm a guess offline.';
