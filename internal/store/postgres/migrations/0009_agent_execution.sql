ALTER TABLE public.conversation_message
    ADD COLUMN client_message_id uuid,
    ADD CONSTRAINT conversation_message_client_id_unique
        UNIQUE (org_id, conversation_id, client_message_id);

CREATE TABLE public.agent_execution (
    org_id uuid NOT NULL,
    execution_id uuid NOT NULL,
    conversation_id uuid,
    investigation_id uuid,
    message_sequence bigint,
    status text NOT NULL DEFAULT 'queued',
    lease_token uuid,
    lease_expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, execution_id),
    UNIQUE (org_id, investigation_id),
    UNIQUE (org_id, conversation_id, message_sequence),
    FOREIGN KEY (org_id, conversation_id, message_sequence)
        REFERENCES public.conversation_message (org_id, conversation_id, sequence) ON DELETE CASCADE,
    FOREIGN KEY (org_id, investigation_id)
        REFERENCES public.investigation (org_id, investigation_id) ON DELETE CASCADE,
    CHECK (
        (conversation_id IS NOT NULL AND message_sequence IS NOT NULL AND investigation_id IS NULL)
        OR (conversation_id IS NULL AND message_sequence IS NULL AND investigation_id IS NOT NULL)
    ),
    CHECK (status IN ('queued', 'running', 'needs_input', 'completed', 'failed', 'cancelled')),
    CHECK ((status = 'running') = (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CHECK ((lease_token IS NULL) = (lease_expires_at IS NULL))
);

CREATE TABLE public.agent_checkpoint (
    org_id uuid NOT NULL,
    session_id text NOT NULL,
    writer_execution_id uuid NOT NULL,
    state jsonb NOT NULL,
    PRIMARY KEY (org_id, session_id),
    FOREIGN KEY (org_id, writer_execution_id)
        REFERENCES public.agent_execution (org_id, execution_id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(state) = 'object')
);
