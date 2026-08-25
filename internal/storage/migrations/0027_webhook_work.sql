-- Durable webhook work separates acknowledgement from domain execution. Only references
-- and bounded classifications are retained here; raw webhook bodies and credentials never
-- cross this boundary.

ALTER TABLE integration_delivery
    ADD CONSTRAINT integration_delivery_identity_is_org_scoped
        UNIQUE (org_id, delivery_id);

ALTER TABLE conversation_message
    ADD COLUMN provider_channel_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN provider_message_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN source_reference TEXT NOT NULL DEFAULT '';

CREATE TABLE webhook_work
(
    work_id          UUID        NOT NULL PRIMARY KEY,
    org_id           TEXT        NOT NULL CHECK (length(org_id) BETWEEN 1 AND 128),
    kind             SMALLINT    NOT NULL CHECK (kind IN (1, 2)),
    status           SMALLINT    NOT NULL DEFAULT 1 CHECK (status IN (1, 2, 3, 4, 5)),
    delivery_id      UUID        NOT NULL,
    integration_id   UUID        NOT NULL,
    episode_id       UUID,
    conversation_id  UUID,
    message_sequence BIGINT,
    attempts         SMALLINT    NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 12),
    available_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_owner      TEXT        NOT NULL DEFAULT '' CHECK (length(lease_owner) <= 128),
    lease_epoch      BIGINT      NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
    lease_expires_at TIMESTAMPTZ,
    failure_class    TEXT        NOT NULL DEFAULT '' CHECK (length(failure_class) <= 64),
    failure_message  TEXT        NOT NULL DEFAULT '' CHECK (length(failure_message) <= 512),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT webhook_work_identity_is_org_scoped UNIQUE (org_id, work_id),
    CONSTRAINT webhook_work_delivery_is_in_the_same_org
        FOREIGN KEY (org_id, delivery_id)
            REFERENCES integration_delivery (org_id, delivery_id) ON DELETE CASCADE,
    CONSTRAINT webhook_work_integration_is_in_the_same_org
        FOREIGN KEY (org_id, integration_id)
            REFERENCES integration (org_id, integration_id),
    CONSTRAINT webhook_work_episode_is_in_the_same_org
        FOREIGN KEY (org_id, episode_id)
            REFERENCES incident_episode (org_id, episode_id),
    CONSTRAINT webhook_work_message_is_in_the_same_org
        FOREIGN KEY (org_id, conversation_id, message_sequence)
            REFERENCES conversation_message (org_id, conversation_id, sequence),
    CONSTRAINT webhook_work_has_one_effect_reference CHECK (
        (kind = 1 AND episode_id IS NOT NULL AND conversation_id IS NULL AND message_sequence IS NULL)
        OR
        (kind = 2 AND episode_id IS NULL AND conversation_id IS NOT NULL AND message_sequence IS NOT NULL)
    ),
    CONSTRAINT webhook_work_lease_is_complete CHECK (
        (status = 2) = (lease_owner <> '' AND lease_expires_at IS NOT NULL)
    ),
    CONSTRAINT webhook_work_failure_matches_retry_or_terminal CHECK (
        (status IN (3, 4) AND failure_class <> '')
        OR (status NOT IN (3, 4) AND failure_class = '')
    )
);

CREATE UNIQUE INDEX webhook_work_source_effect_is_unique
    ON webhook_work (org_id, kind, delivery_id, COALESCE(episode_id, conversation_id),
                     COALESCE(message_sequence, 0));
CREATE INDEX webhook_work_ready_idx
    ON webhook_work (available_at, created_at, work_id)
    WHERE status IN (1, 2, 3);
CREATE INDEX webhook_work_terminal_idx
    ON webhook_work (org_id, updated_at DESC, work_id DESC)
    WHERE status = 4;

ALTER TABLE investigation
    ADD COLUMN webhook_work_id UUID;
ALTER TABLE investigation
    ADD CONSTRAINT investigation_webhook_work_is_in_the_same_org
        FOREIGN KEY (org_id, webhook_work_id)
            REFERENCES webhook_work (org_id, work_id);
CREATE UNIQUE INDEX investigation_webhook_work_is_unique
    ON investigation (org_id, webhook_work_id)
    WHERE webhook_work_id IS NOT NULL;

COMMENT ON COLUMN investigation_tool_run.capability IS
    'Legacy compatibility field. New writes leave it empty; Tool is the investigation operation.';
