LOCK TABLE integration_type, integration, integration_connect_flow, integration_installation IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM integration_type t
        LEFT JOIN (VALUES
            (1, 'alertmanager'), (2, 'kubernetes'), (3, 'slack'),
            (4, 'github'), (5, 'generic_webhook')
        ) AS expected(id, key) ON expected.id = t.integration_type_id
        WHERE expected.id IS NULL OR t.key <> expected.key
    ) THEN
        RAISE EXCEPTION 'integration catalog contains unknown or conflicting kind identities; reconcile retained catalog rows before upgrading';
    END IF;

    IF EXISTS (
        SELECT 1 FROM integration_installation s
        LEFT JOIN integration i ON i.org_id = s.org_id AND i.integration_id = s.integration_id
        WHERE i.integration_id IS NULL OR i.integration_type_id <> s.integration_type_id
    ) THEN
        RAISE EXCEPTION 'integration installation disagrees with its parent Integration; reconcile retained routing before upgrading';
    END IF;
END $$;

ALTER TABLE integration
    ADD CONSTRAINT integration_supported_kind CHECK (integration_type_id IN (1, 2, 3, 4, 5)),
    ADD CONSTRAINT integration_org_id_kind_unique UNIQUE (org_id, integration_id, integration_type_id),
    DROP CONSTRAINT integration_integration_type_id_fkey;

ALTER TABLE integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_supported_kind CHECK (integration_type_id IN (1, 2, 3, 4, 5)),
    DROP CONSTRAINT integration_connect_flow_integration_type_id_fkey;

ALTER TABLE integration_installation
    ADD CONSTRAINT integration_installation_supported_kind CHECK (integration_type_id IN (1, 2, 3, 4, 5)),
    ADD CONSTRAINT integration_installation_matches_parent_kind
        FOREIGN KEY (org_id, integration_id, integration_type_id)
        REFERENCES integration (org_id, integration_id, integration_type_id) ON DELETE CASCADE,
    DROP CONSTRAINT integration_installation_is_in_the_same_org,
    DROP CONSTRAINT integration_installation_integration_type_id_fkey;

DROP TABLE integration_type;
