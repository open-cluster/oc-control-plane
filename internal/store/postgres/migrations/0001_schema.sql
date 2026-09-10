CREATE FUNCTION audit_event_is_append_only() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' AND
       coalesce(current_setting('opencluster.audit_retention', TRUE), '') = 'pruning' THEN
        RETURN NULL;
    END IF;
    RAISE EXCEPTION 'audit_event is append-only; % is refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE FUNCTION prevent_slack_conversation_retargeting() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.org_id, NEW.conversation_id, NEW.integration_id, NEW.channel_id, NEW.thread_ts)
        IS DISTINCT FROM (OLD.org_id, OLD.conversation_id, OLD.integration_id, OLD.channel_id, OLD.thread_ts) THEN
        RAISE EXCEPTION 'Slack Conversation routing is immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE TABLE alert_event (
    alert_event_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    source_key text NOT NULL,
    status smallint NOT NULL, -- 1=firing, 2=resolved
    title text NOT NULL,
    summary text NOT NULL,
    labels jsonb DEFAULT '{}'::jsonb NOT NULL,
    started_at timestamptz NOT NULL,
    resolved_at timestamptz,
    received_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    incident_id uuid,
    annotations jsonb DEFAULT '{}'::jsonb NOT NULL,
    generator_url text DEFAULT '' NOT NULL,
    CONSTRAINT alert_event_resolution_follows_its_start CHECK ((resolved_at IS NULL) OR (resolved_at >= started_at)),
    CONSTRAINT alert_event_resolution_is_stamped CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT alert_event_status_check CHECK (status = ANY (ARRAY[1, 2])),
    CONSTRAINT alert_event_incident_is_unique UNIQUE (integration_id, source_key, started_at),
    CONSTRAINT alert_event_pkey PRIMARY KEY (alert_event_id)
);

CREATE TABLE app_user (
    user_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    email text NOT NULL,
    email_verified boolean DEFAULT false NOT NULL,
    display_name text DEFAULT '' NOT NULL,
    disabled_at timestamptz,
    last_sign_in timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT app_user_identity_is_the_issuer_and_subject UNIQUE (issuer, subject),
    CONSTRAINT app_user_pkey PRIMARY KEY (user_id)
);

CREATE TABLE audit_event (
    event_id uuid NOT NULL,
    org_id uuid,
    actor_kind smallint NOT NULL, -- 1=user, 3=system
    actor_id text DEFAULT '' NOT NULL,
    actor_display_name text DEFAULT '' NOT NULL,
    action text NOT NULL,
    target_kind text DEFAULT '' NOT NULL,
    target_id text DEFAULT '' NOT NULL,
    outcome smallint NOT NULL, -- 1=allowed, 2=denied, 3=failed
    source_address text DEFAULT '' NOT NULL,
    request_id text DEFAULT '' NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT audit_event_actor_kind_check CHECK (actor_kind = ANY (ARRAY[1, 3])),
    CONSTRAINT audit_event_deployment_action_check CHECK ((org_id IS NOT NULL) OR (action = ANY (ARRAY['session.signed-out', 'session.revoked', 'local.password-changed', 'local.password-recovered', 'local.bootstrap-completed']))),
    CONSTRAINT audit_event_outcome_check CHECK (outcome = ANY (ARRAY[1, 2, 3])),
    CONSTRAINT audit_event_pkey PRIMARY KEY (event_id)
);

CREATE TABLE change_ledger (
    entry_id bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    namespace text NOT NULL,
    object_kind smallint NOT NULL, -- 1=Deployment, 2=StatefulSet, 3=DaemonSet, 4=ConfigMap, 5=Secret
    object_name text NOT NULL,
    object_uid text NOT NULL,
    observed_revision text NOT NULL,
    change_kind smallint NOT NULL, -- 1=baseline, 2=created, 3=modified, 4=deleted
    observed_at timestamptz NOT NULL,
    received_at timestamptz DEFAULT now() NOT NULL,
    fields jsonb DEFAULT '[]'::jsonb NOT NULL,
    CONSTRAINT change_ledger_change_kind_check CHECK (change_kind = ANY (ARRAY[1, 2, 3, 4])),
    CONSTRAINT change_ledger_deletion_has_no_revision CHECK ((change_kind = 4) = (observed_revision = '')),
    CONSTRAINT change_ledger_object_kind_check CHECK (object_kind = ANY (ARRAY[1, 2, 3, 4, 5])),
    CONSTRAINT change_ledger_entry_is_unique_per_observation UNIQUE (integration_id, object_uid, observed_revision),
    CONSTRAINT change_ledger_pkey PRIMARY KEY (entry_id)
);

CREATE TABLE change_ledger_scope (
    integration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    policy_revision bigint DEFAULT 1 NOT NULL,
    requested_interval_seconds integer NOT NULL,
    covered_since timestamptz,
    baseline_at timestamptz,
    last_confirmed_at timestamptz,
    faulted boolean DEFAULT false NOT NULL,
    truncated boolean DEFAULT false NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT change_ledger_scope_requested_interval_seconds_check CHECK (requested_interval_seconds > 0),
    CONSTRAINT change_ledger_scope_pkey PRIMARY KEY (integration_id)
);

CREATE TABLE conversation (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    incident_id uuid,
    surface smallint DEFAULT 1 NOT NULL, -- 1=web, 2=Slack
    subject text NOT NULL,
    state smallint DEFAULT 1 NOT NULL, -- 1=open, 2=closed
    created_by text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    last_activity_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT conversation_state_check CHECK (state = ANY (ARRAY[1, 2])),
    CONSTRAINT conversation_surface_check CHECK (surface = ANY (ARRAY[1, 2])),
    CONSTRAINT conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id),
    CONSTRAINT conversation_pkey PRIMARY KEY (conversation_id)
);

CREATE TABLE conversation_message (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    sequence bigint NOT NULL,
    role smallint NOT NULL, -- 1=person, 2=agent
    actor_kind smallint NOT NULL, -- 1=Principal, 2=external
    actor_id text DEFAULT '' NOT NULL,
    actor_display text DEFAULT '' NOT NULL,
    text text NOT NULL,
    investigation_id uuid,
    created_at timestamptz DEFAULT now() NOT NULL,
    provider_channel_id text DEFAULT '' NOT NULL,
    provider_message_id text DEFAULT '' NOT NULL,
    source_reference text DEFAULT '' NOT NULL,
    window_from timestamptz NOT NULL,
    window_until timestamptz NOT NULL,
    CONSTRAINT conversation_message_actor_kind_check CHECK (actor_kind = ANY (ARRAY[1, 2])),
    CONSTRAINT conversation_message_role_check CHECK (role = ANY (ARRAY[1, 2])),
    CONSTRAINT conversation_message_sequence_check CHECK (sequence >= 1),
    CONSTRAINT conversation_message_window_is_whole CHECK (window_from < window_until),
    CONSTRAINT conversation_message_pkey PRIMARY KEY (org_id, conversation_id, sequence)
);

CREATE TABLE deployment_initialization (
    singleton boolean DEFAULT true NOT NULL,
    initialized_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT deployment_initialization_singleton_check CHECK (singleton),
    CONSTRAINT deployment_initialization_pkey PRIMARY KEY (singleton)
);

CREATE TABLE deployment_sign_in_flow (
    flow_id uuid NOT NULL,
    org_id uuid NOT NULL,
    state_digest bytea NOT NULL,
    code_verifier text,
    nonce text,
    return_to text NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT deployment_sign_in_flow_state_digest_check CHECK (octet_length(state_digest) = 32),
    CONSTRAINT deployment_sign_in_flow_pkey PRIMARY KEY (flow_id),
    CONSTRAINT deployment_sign_in_flow_state_digest_key UNIQUE (state_digest)
);

CREATE TABLE incident (
    incident_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    grouping_key text NOT NULL,
    grouping_basis smallint NOT NULL, -- 1=source grouping, 2=ungrouped
    title text NOT NULL,
    status smallint NOT NULL, -- 1=open, 2=resolved
    first_seen_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    resolved_at timestamptz,
    superseded_by uuid,
    superseded_at timestamptz,
    supersede_reason text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT incident_ends_after_it_starts CHECK (last_seen_at >= first_seen_at),
    CONSTRAINT incident_grouping_basis_check CHECK (grouping_basis = ANY (ARRAY[1, 2])),
    CONSTRAINT incident_resolution_is_stamped CHECK ((status = 2) = (resolved_at IS NOT NULL)),
    CONSTRAINT incident_status_check CHECK (status = ANY (ARRAY[1, 2])),
    CONSTRAINT incident_supersedes_something_else CHECK ((superseded_by IS NULL) OR (superseded_by <> incident_id)),
    CONSTRAINT incident_supersession_is_stamped CHECK ((superseded_by IS NULL) = (superseded_at IS NULL)),
    CONSTRAINT incident_identity_is_org_scoped UNIQUE (org_id, incident_id),
    CONSTRAINT incident_pkey PRIMARY KEY (incident_id)
);

CREATE TABLE integration (
    integration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_type_id smallint NOT NULL, -- 1=Alertmanager, 2=Kubernetes, 3=Slack, 4=GitHub, 5=generic webhook
    name text NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    webhook_secret_digest bytea,
    webhook_secret_fingerprint text,
    webhook_secret_created_at timestamptz,
    webhook_secret_rotated_at timestamptz,
    labels jsonb DEFAULT '{}'::jsonb NOT NULL,
    relay_id uuid,
    status smallint DEFAULT 1 NOT NULL, -- 1=configured, 2=active, 3=degraded, 4=failed
    last_verified_at timestamptz,
    verify_note text DEFAULT '' NOT NULL,
    disabled_at timestamptz,
    created_by text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    credential_sealed bytea,
    credential_fingerprint text,
    credential_created_at timestamptz,
    credential_rotated_at timestamptz,
    verify_grants jsonb,
    verify_facts jsonb,
    CONSTRAINT integration_credential_is_whole CHECK (((credential_sealed IS NULL) AND (credential_fingerprint IS NULL) AND (credential_created_at IS NULL)) OR ((credential_sealed IS NOT NULL) AND (credential_fingerprint IS NOT NULL) AND (credential_created_at IS NOT NULL))),
    CONSTRAINT integration_status_check CHECK (status = ANY (ARRAY[1, 2, 3, 4])),
    CONSTRAINT integration_supported_kind CHECK (integration_type_id = ANY (ARRAY[1, 2, 3, 4, 5])),
    CONSTRAINT integration_webhook_secret_digest_check CHECK ((webhook_secret_digest IS NULL) OR (length(webhook_secret_digest) = 32)),
    CONSTRAINT integration_webhook_secret_is_whole CHECK (((webhook_secret_digest IS NULL) AND (webhook_secret_fingerprint IS NULL) AND (webhook_secret_created_at IS NULL)) OR ((webhook_secret_digest IS NOT NULL) AND (webhook_secret_fingerprint IS NOT NULL) AND (webhook_secret_created_at IS NOT NULL))),
    CONSTRAINT integration_identity_is_org_scoped UNIQUE (org_id, integration_id),
    CONSTRAINT integration_name_is_unique_per_org UNIQUE (org_id, name),
    CONSTRAINT integration_org_id_kind_unique UNIQUE (org_id, integration_id, integration_type_id),
    CONSTRAINT integration_pkey PRIMARY KEY (integration_id)
);

CREATE TABLE integration_connect_flow (
    flow_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_type_id smallint NOT NULL, -- 1=Alertmanager, 2=Kubernetes, 3=Slack, 4=GitHub, 5=generic webhook
    principal text NOT NULL,
    state_digest bytea NOT NULL,
    return_to text DEFAULT '/' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    CONSTRAINT integration_connect_flow_expires_after_it_started CHECK (expires_at > created_at),
    CONSTRAINT integration_connect_flow_state_digest_check CHECK (length(state_digest) = 32),
    CONSTRAINT integration_connect_flow_supported_kind CHECK (integration_type_id = ANY (ARRAY[1, 2, 3, 4, 5])),
    CONSTRAINT integration_connect_flow_pkey PRIMARY KEY (flow_id),
    CONSTRAINT integration_connect_flow_state_is_unique UNIQUE (state_digest)
);

CREATE TABLE webhook_delivery (
    delivery_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    outcome smallint NOT NULL, -- 1=accepted, 2=duplicate, 3=rejected
    body_digest bytea,
    reason text DEFAULT '' NOT NULL,
    alert_event_count integer DEFAULT 0 NOT NULL,
    truncated integer DEFAULT 0 NOT NULL,
    received_at timestamptz DEFAULT now() NOT NULL,
    provider_identity text,
    lifecycle_phase text,
    request_id text DEFAULT '' NOT NULL,
    CONSTRAINT webhook_delivery_accepted_carries_a_digest CHECK ((outcome <> 1) OR (body_digest IS NOT NULL)),
    CONSTRAINT webhook_delivery_accepted_carries_provider_identity CHECK ((outcome <> 1) OR ((provider_identity IS NOT NULL) AND (lifecycle_phase IS NOT NULL))),
    CONSTRAINT webhook_delivery_alert_event_count_check CHECK (alert_event_count >= 0),
    CONSTRAINT webhook_delivery_body_digest_check CHECK ((body_digest IS NULL) OR (length(body_digest) = 32)),
    CONSTRAINT webhook_delivery_lifecycle_phase_check CHECK ((lifecycle_phase IS NULL) OR (lifecycle_phase = ANY (ARRAY['', 'firing', 'resolved']))),
    CONSTRAINT webhook_delivery_nonaccepted_has_no_provider_identity CHECK ((outcome = 1) OR ((provider_identity IS NULL) AND (lifecycle_phase IS NULL))),
    CONSTRAINT webhook_delivery_outcome_check CHECK (outcome = ANY (ARRAY[1, 2, 3])),
    CONSTRAINT webhook_delivery_states_a_reason_exactly_when_it_refused CHECK ((outcome = 3) = (reason <> '')),
    CONSTRAINT webhook_delivery_truncated_check CHECK (truncated >= 0),
    CONSTRAINT webhook_delivery_identity_is_org_scoped UNIQUE (org_id, delivery_id),
    CONSTRAINT webhook_delivery_pkey PRIMARY KEY (delivery_id)
);

CREATE TABLE integration_installation (
    integration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_type_id smallint NOT NULL, -- 1=Alertmanager, 2=Kubernetes, 3=Slack, 4=GitHub, 5=generic webhook
    application text NOT NULL,
    enterprise text DEFAULT '' NOT NULL,
    workspace text NOT NULL,
    enterprise_wide boolean DEFAULT false NOT NULL,
    agent text DEFAULT '' NOT NULL,
    authorizer text DEFAULT '' NOT NULL,
    grants text[] DEFAULT '{}' NOT NULL,
    installed_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT integration_installation_supported_kind CHECK (integration_type_id = ANY (ARRAY[1, 2, 3, 4, 5])),
    CONSTRAINT integration_installation_pkey PRIMARY KEY (integration_id)
);

CREATE TABLE investigation (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    incident_id uuid,
    question text DEFAULT '' NOT NULL,
    subject text NOT NULL,
    window_from timestamptz NOT NULL,
    window_until timestamptz NOT NULL,
    status smallint DEFAULT 1 NOT NULL, -- 1=running, 2=concluded, 3=failed, 4=cancelled
    conclusion jsonb DEFAULT '{}'::jsonb NOT NULL,
    error text DEFAULT '' NOT NULL,
    spend_input_tokens bigint DEFAULT 0 NOT NULL,
    spend_output_tokens bigint DEFAULT 0 NOT NULL,
    created_by text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    concluded_at timestamptz,
    stopped_by text DEFAULT '' NOT NULL,
    conversation_id uuid,
    turn smallint,
    lease_worker text DEFAULT '' NOT NULL,
    lease_expires_at timestamptz,
    webhook_job_id uuid,
    cancel_requested_at timestamptz,
    cancelled_by text DEFAULT '' NOT NULL,
    lease_token uuid,
    CONSTRAINT investigation_cancellation_is_attributed CHECK ((status = 4) = ((cancel_requested_at IS NOT NULL) AND (cancelled_by <> ''))),
    CONSTRAINT investigation_conclusion_is_stamped CHECK ((status = 1) = (concluded_at IS NULL)),
    CONSTRAINT investigation_failure_states_a_reason CHECK ((status = 3) = (error <> '')),
    CONSTRAINT investigation_lease_is_whole CHECK ((lease_worker = '') = (lease_expires_at IS NULL)),
    CONSTRAINT investigation_lease_token_is_whole CHECK ((lease_worker = '') = (lease_token IS NULL)),
    CONSTRAINT investigation_spend_input_tokens_check CHECK (spend_input_tokens >= 0),
    CONSTRAINT investigation_spend_output_tokens_check CHECK (spend_output_tokens >= 0),
    CONSTRAINT investigation_status_check CHECK (status = ANY (ARRAY[1, 2, 3, 4])),
    CONSTRAINT investigation_stop_is_a_conclusion CHECK ((stopped_by = '') OR (status = 2)),
    CONSTRAINT investigation_turn_belongs_to_a_conversation CHECK ((conversation_id IS NULL) = (turn IS NULL)),
    CONSTRAINT investigation_turn_check CHECK (turn >= 1),
    CONSTRAINT investigation_window_ends_after_it_starts CHECK (window_until >= window_from),
    CONSTRAINT investigation_identity_in_conversation UNIQUE (org_id, conversation_id, investigation_id),
    CONSTRAINT investigation_identity_is_org_scoped UNIQUE (org_id, investigation_id),
    CONSTRAINT investigation_pkey PRIMARY KEY (investigation_id),
    CONSTRAINT investigation_turn_is_unique_in_its_conversation UNIQUE (org_id, conversation_id, turn)
);

CREATE TABLE investigation_event (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    sequence bigint NOT NULL,
    at timestamptz DEFAULT now() NOT NULL,
    type smallint NOT NULL, -- 1=started, 2=progress, 3=tool started, 4=tool completed, 6=concluded, 7=failed, 9=cancelled, 10=hypotheses updated
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT investigation_event_sequence_check CHECK (sequence >= 1),
    CONSTRAINT investigation_event_type_check CHECK ((type >= 1) AND (type <= 10)),
    CONSTRAINT investigation_event_pkey PRIMARY KEY (investigation_id, sequence)
);

CREATE TABLE investigation_tool_run (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid,
    ordinal smallint NOT NULL,
    tool text NOT NULL,
    arguments jsonb DEFAULT '{}'::jsonb NOT NULL,
    window_from timestamptz NOT NULL,
    window_until timestamptz NOT NULL,
    outcome smallint NOT NULL, -- 1=succeeded, 2=failed
    truncated boolean DEFAULT false NOT NULL,
    summary text DEFAULT '' NOT NULL,
    sources jsonb DEFAULT '[]'::jsonb NOT NULL,
    error text DEFAULT '' NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    purpose text DEFAULT '' NOT NULL,
    hypothesis_id text DEFAULT '' NOT NULL,
    CONSTRAINT investigation_tool_run_failure_states_a_reason CHECK ((outcome = 2) = (error <> '')),
    CONSTRAINT investigation_tool_run_ordinal_check CHECK (ordinal >= 1),
    CONSTRAINT investigation_tool_run_outcome_check CHECK (outcome = ANY (ARRAY[1, 2])),
    CONSTRAINT investigation_tool_run_pkey PRIMARY KEY (investigation_id, ordinal)
);

CREATE TABLE local_password (
    user_id uuid NOT NULL,
    password_hash text NOT NULL,
    password_changed_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT local_password_password_hash_check CHECK ((length(password_hash) >= 32) AND (length(password_hash) <= 512)),
    CONSTRAINT local_password_pkey PRIMARY KEY (user_id)
);

CREATE TABLE session (
    session_id uuid NOT NULL,
    credential_digest bytea NOT NULL,
    user_id uuid NOT NULL,
    org_id uuid,
    issued_at timestamptz DEFAULT now() NOT NULL,
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz DEFAULT now() NOT NULL,
    revoked_at timestamptz,
    revoked_by text DEFAULT '' NOT NULL,
    user_agent text DEFAULT '' NOT NULL,
    address text DEFAULT '' NOT NULL,
    CONSTRAINT session_credential_digest_check CHECK (length(credential_digest) = 32),
    CONSTRAINT session_expires_after_it_was_issued CHECK (expires_at > issued_at),
    CONSTRAINT session_credential_digest_is_unique UNIQUE (credential_digest),
    CONSTRAINT session_pkey PRIMARY KEY (session_id)
);

CREATE TABLE organization (
    org_id uuid DEFAULT gen_random_uuid() NOT NULL,
    display_name text NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    audit_retention_days integer DEFAULT 90 NOT NULL,
    CONSTRAINT organization_audit_retention_days_check CHECK (audit_retention_days >= 0),
    CONSTRAINT organization_pkey PRIMARY KEY (org_id)
);

CREATE TABLE organization_membership (
    membership_id uuid NOT NULL,
    org_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    source smallint NOT NULL, -- 1=manual, 2=JIT, 3=SCIM
    external_id text,
    active boolean DEFAULT true NOT NULL,
    granted_by text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT organization_membership_role_check CHECK (role = ANY (ARRAY['admin', 'editor', 'viewer'])),
    CONSTRAINT organization_membership_source_check CHECK (source = ANY (ARRAY[1, 2, 3])),
    CONSTRAINT organization_membership_is_one_per_tenant UNIQUE (org_id, user_id),
    CONSTRAINT organization_membership_pkey PRIMARY KEY (membership_id)
);

CREATE TABLE postmortem (
    incident_id uuid NOT NULL,
    org_id uuid NOT NULL,
    status text NOT NULL,
    revision integer NOT NULL,
    document jsonb NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    reviewed_at timestamptz,
    reviewed_by text DEFAULT '' NOT NULL,
    CONSTRAINT postmortem_review_is_stamped CHECK ((status = 'reviewed') = (reviewed_at IS NOT NULL)),
    CONSTRAINT postmortem_revision_check CHECK (revision >= 1),
    CONSTRAINT postmortem_status_check CHECK (status = ANY (ARRAY['draft', 'reviewed'])),
    CONSTRAINT postmortem_pkey PRIMARY KEY (org_id, incident_id)
);

CREATE TABLE relay_bootstrap_token (
    bootstrap_digest bytea NOT NULL,
    org_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT relay_bootstrap_token_bootstrap_digest_check CHECK (length(bootstrap_digest) = 32),
    CONSTRAINT relay_bootstrap_token_pkey PRIMARY KEY (bootstrap_digest)
);

CREATE TABLE relay_job (
    job_id uuid NOT NULL,
    org_id uuid NOT NULL,
    registration_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    capability_id text NOT NULL,
    capability_version integer NOT NULL,
    arguments bytea NOT NULL,
    status smallint DEFAULT 0 NOT NULL, -- 0=pending, 1=leased, 2=succeeded, 3=failed, 4=cancelled
    lease_session uuid,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamptz,
    cancel_requested_at timestamptz,
    result bytea,
    terminal_at timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    investigation_id uuid,
    CONSTRAINT relay_job_lease_is_whole CHECK (((lease_session IS NULL) AND (lease_expires_at IS NULL)) OR ((lease_session IS NOT NULL) AND (lease_expires_at IS NOT NULL))),
    CONSTRAINT relay_job_status_check CHECK ((status >= 0) AND (status <= 4)),
    CONSTRAINT relay_job_terminal_is_stamped CHECK ((status = ANY (ARRAY[2, 3, 4])) = (terminal_at IS NOT NULL)),
    CONSTRAINT relay_job_pkey PRIMARY KEY (job_id)
);

CREATE TABLE relay_registration (
    registration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    credential_digest bytea NOT NULL,
    cluster_fingerprint text NOT NULL,
    relay_version text NOT NULL,
    protocol_version bigint,
    capabilities jsonb NOT NULL,
    revoked_at timestamptz,
    session_conflict_at timestamptz,
    session_conflict_hosts integer DEFAULT 0 NOT NULL,
    session_id uuid,
    session_started_at timestamptz,
    session_ended_at timestamptz,
    last_seen_at timestamptz,
    session_peer text,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT relay_registration_credential_digest_check CHECK (length(credential_digest) = 32),
    CONSTRAINT relay_registration_protocol_version_check CHECK ((protocol_version IS NULL) OR ((protocol_version >= 1) AND (protocol_version <= '4294967295'::bigint))),
    CONSTRAINT relay_registration_identity_is_org_scoped UNIQUE (org_id, registration_id),
    CONSTRAINT relay_registration_pkey PRIMARY KEY (registration_id)
);

CREATE TABLE slack_conversation (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT slack_conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id),
    CONSTRAINT slack_conversation_is_one_thread UNIQUE (integration_id, channel_id, thread_ts),
    CONSTRAINT slack_conversation_pkey PRIMARY KEY (conversation_id)
);

CREATE TABLE slack_reply (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    stream_ts text DEFAULT '' NOT NULL,
    native boolean DEFAULT false NOT NULL,
    status smallint DEFAULT 1 NOT NULL, -- 1=pending, 2=delivering, 3=delivered, 4=failed
    last_sequence bigint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamptz DEFAULT now() NOT NULL,
    note text DEFAULT '' NOT NULL,
    leased_until timestamptz,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    lease_owner uuid,
    CONSTRAINT slack_reply_attempts_check CHECK (attempts >= 0),
    CONSTRAINT slack_reply_last_sequence_check CHECK (last_sequence >= 0),
    CONSTRAINT slack_reply_lease_is_whole CHECK ((leased_until IS NULL) = (lease_owner IS NULL)),
    CONSTRAINT slack_reply_status_check CHECK (status = ANY (ARRAY[1, 2, 3, 4])),
    CONSTRAINT slack_reply_pkey PRIMARY KEY (investigation_id)
);

CREATE TABLE webhook_job (
    job_id uuid NOT NULL,
    org_id uuid NOT NULL,
    kind smallint NOT NULL, -- 1=Alert Event, 2=Slack Message
    status smallint DEFAULT 1 NOT NULL, -- 1=ready, 2=leased, 3=retry, 4=terminal, 5=complete
    delivery_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    incident_id uuid,
    conversation_id uuid,
    message_sequence bigint,
    attempts smallint DEFAULT 0 NOT NULL,
    available_at timestamptz DEFAULT now() NOT NULL,
    lease_owner text DEFAULT '' NOT NULL,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamptz,
    failure_class text DEFAULT '' NOT NULL,
    failure_message text DEFAULT '' NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT webhook_job_attempts_check CHECK ((attempts >= 0) AND (attempts <= 12)),
    CONSTRAINT webhook_job_failure_matches_retry_or_terminal CHECK (((status = ANY (ARRAY[3, 4])) AND (failure_class <> '')) OR ((status <> ALL (ARRAY[3, 4])) AND (failure_class = ''))),
    CONSTRAINT webhook_job_has_one_effect_reference CHECK (((kind = 1) AND (incident_id IS NOT NULL) AND (conversation_id IS NULL) AND (message_sequence IS NULL)) OR ((kind = 2) AND (incident_id IS NULL) AND (conversation_id IS NOT NULL) AND (message_sequence IS NOT NULL))),
    CONSTRAINT webhook_job_kind_check CHECK (kind = ANY (ARRAY[1, 2])),
    CONSTRAINT webhook_job_lease_epoch_check CHECK (lease_epoch >= 0),
    CONSTRAINT webhook_job_lease_is_complete CHECK ((status = 2) = ((lease_owner <> '') AND (lease_expires_at IS NOT NULL))),
    CONSTRAINT webhook_job_status_check CHECK (status = ANY (ARRAY[1, 2, 3, 4, 5])),
    CONSTRAINT webhook_job_identity_is_org_scoped UNIQUE (org_id, job_id),
    CONSTRAINT webhook_job_pkey PRIMARY KEY (job_id)
);

CREATE INDEX alert_event_incident_idx ON alert_event (incident_id, started_at DESC);

CREATE INDEX alert_event_org_idx ON alert_event (org_id, received_at DESC);

CREATE INDEX alert_event_source_key_idx ON alert_event (integration_id, source_key, started_at DESC);

CREATE INDEX app_user_email_idx ON app_user (lower(email));

CREATE INDEX audit_event_actor_idx ON audit_event (org_id, actor_id, occurred_at DESC);

CREATE INDEX audit_event_org_idx ON audit_event (org_id, occurred_at DESC, event_id DESC);

CREATE INDEX audit_event_target_idx ON audit_event (org_id, target_kind, target_id, occurred_at DESC, event_id DESC);

CREATE INDEX change_ledger_retention_idx ON change_ledger (org_id, received_at);

CREATE INDEX change_ledger_window_idx ON change_ledger (integration_id, namespace, observed_at);

CREATE INDEX conversation_incident_idx ON conversation (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX conversation_message_assigned_idx ON conversation_message (org_id, investigation_id, conversation_id, sequence) WHERE ((role = 1) AND (investigation_id IS NOT NULL));

CREATE INDEX conversation_message_queued_idx ON conversation_message (org_id, conversation_id, sequence) WHERE (investigation_id IS NULL);

CREATE INDEX conversation_org_idx ON conversation (org_id, last_activity_at DESC, conversation_id DESC);

CREATE INDEX deployment_sign_in_flow_expiry ON deployment_sign_in_flow (expires_at);

CREATE UNIQUE INDEX incident_open_key_idx ON incident (integration_id, grouping_key) WHERE (status = 1);

CREATE INDEX incident_org_idx ON incident (org_id, last_seen_at DESC, incident_id DESC);

CREATE INDEX integration_connect_flow_expiry_idx ON integration_connect_flow (expires_at);

CREATE INDEX webhook_delivery_accepted_idx ON webhook_delivery (integration_id, received_at DESC) WHERE (outcome = 1);

CREATE UNIQUE INDEX webhook_delivery_accepted_provider_identity_is_unique ON webhook_delivery (integration_id, provider_identity, lifecycle_phase) WHERE (outcome = 1);

CREATE INDEX webhook_delivery_integration_idx ON webhook_delivery (org_id, integration_id, received_at DESC, delivery_id DESC);

CREATE UNIQUE INDEX integration_installation_is_one_workspace ON integration_installation (integration_type_id, application, enterprise, workspace);

CREATE INDEX integration_org_idx ON integration (org_id, created_at DESC);

CREATE INDEX integration_relay_idx ON integration (org_id, relay_id) WHERE (relay_id IS NOT NULL);

CREATE INDEX investigation_claimable_idx ON investigation (org_id, created_at, investigation_id) WHERE (status = 1);

CREATE INDEX investigation_incident_idx ON investigation (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX investigation_lease_expiry_idx ON investigation (lease_expires_at) WHERE ((status = 1) AND (lease_worker <> ''));

CREATE UNIQUE INDEX investigation_one_running_per_conversation ON investigation (org_id, conversation_id) WHERE ((conversation_id IS NOT NULL) AND (status = 1));

CREATE INDEX investigation_org_idx ON investigation (org_id, created_at DESC, investigation_id DESC);

CREATE UNIQUE INDEX investigation_webhook_job_is_unique ON investigation (org_id, webhook_job_id) WHERE (webhook_job_id IS NOT NULL);

CREATE INDEX session_expiry_idx ON session (expires_at);

CREATE INDEX session_user_idx ON session (user_id, issued_at DESC);

CREATE UNIQUE INDEX organization_membership_external_id_is_unique_per_org ON organization_membership (org_id, external_id) WHERE (external_id IS NOT NULL);

CREATE INDEX organization_membership_org_idx ON organization_membership (org_id, created_at DESC);

CREATE INDEX organization_membership_user_idx ON organization_membership (user_id);

CREATE INDEX relay_job_active_investigation_idx ON relay_job (org_id, investigation_id) WHERE ((investigation_id IS NOT NULL) AND (status = ANY (ARRAY[0, 1])));

CREATE INDEX relay_job_claimable_idx ON relay_job (org_id, registration_id, status, lease_expires_at) WHERE (status = ANY (ARRAY[0, 1]));

CREATE INDEX relay_registration_org_idx ON relay_registration (org_id, created_at DESC);

CREATE INDEX relay_registration_presence_idx ON relay_registration (org_id, last_seen_at DESC);

CREATE INDEX slack_reply_due ON slack_reply (next_attempt_at, investigation_id) WHERE (status = ANY (ARRAY[1, 2]));

CREATE INDEX webhook_job_ready_idx ON webhook_job (available_at, created_at, job_id) WHERE (status = ANY (ARRAY[1, 2, 3]));

CREATE UNIQUE INDEX webhook_job_source_effect_is_unique ON webhook_job (org_id, kind, delivery_id, COALESCE(incident_id, conversation_id), COALESCE(message_sequence, (0)::bigint));

CREATE INDEX webhook_job_terminal_idx ON webhook_job (org_id, updated_at DESC, job_id DESC) WHERE (status = 4);

CREATE TRIGGER audit_event_refuses_delete BEFORE DELETE ON audit_event FOR EACH STATEMENT EXECUTE FUNCTION audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_truncate BEFORE TRUNCATE ON audit_event FOR EACH STATEMENT EXECUTE FUNCTION audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_update BEFORE UPDATE ON audit_event FOR EACH STATEMENT EXECUTE FUNCTION audit_event_is_append_only();

CREATE TRIGGER slack_conversation_routing_is_immutable BEFORE UPDATE ON slack_conversation FOR EACH ROW EXECUTE FUNCTION prevent_slack_conversation_retargeting();

-- Foreign keys are grouped here because several durable records refer to each other cyclically.
ALTER TABLE alert_event
    ADD CONSTRAINT alert_event_incident_id_fkey FOREIGN KEY (incident_id) REFERENCES incident(incident_id);

ALTER TABLE alert_event
    ADD CONSTRAINT alert_event_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE audit_event
    ADD CONSTRAINT audit_event_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE change_ledger
    ADD CONSTRAINT change_ledger_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE change_ledger_scope
    ADD CONSTRAINT change_ledger_scope_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE conversation
    ADD CONSTRAINT conversation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES incident(org_id, incident_id);

ALTER TABLE conversation_message
    ADD CONSTRAINT conversation_message_belongs_to_its_conversation FOREIGN KEY (org_id, conversation_id) REFERENCES conversation(org_id, conversation_id);

ALTER TABLE conversation_message
    ADD CONSTRAINT conversation_message_names_an_org_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES investigation(org_id, investigation_id);

ALTER TABLE conversation
    ADD CONSTRAINT conversation_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE deployment_sign_in_flow
    ADD CONSTRAINT deployment_sign_in_flow_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE incident
    ADD CONSTRAINT incident_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE incident
    ADD CONSTRAINT incident_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES incident(incident_id);

ALTER TABLE integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE webhook_delivery
    ADD CONSTRAINT webhook_delivery_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE integration_installation
    ADD CONSTRAINT integration_installation_matches_parent_kind FOREIGN KEY (org_id, integration_id, integration_type_id) REFERENCES integration(org_id, integration_id, integration_type_id);

ALTER TABLE integration
    ADD CONSTRAINT integration_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE integration
    ADD CONSTRAINT integration_relay_is_in_the_same_org FOREIGN KEY (org_id, relay_id) REFERENCES relay_registration(org_id, registration_id);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES conversation(org_id, conversation_id);

ALTER TABLE investigation_event
    ADD CONSTRAINT investigation_event_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES investigation(org_id, investigation_id);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES incident(org_id, incident_id);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES investigation(org_id, investigation_id);

ALTER TABLE investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_names_an_org_integration FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE investigation
    ADD CONSTRAINT investigation_webhook_job_is_in_the_same_org FOREIGN KEY (org_id, webhook_job_id) REFERENCES webhook_job(org_id, job_id);

ALTER TABLE local_password
    ADD CONSTRAINT local_password_user_id_fkey FOREIGN KEY (user_id) REFERENCES app_user(user_id);

ALTER TABLE session
    ADD CONSTRAINT session_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE session
    ADD CONSTRAINT session_user_id_fkey FOREIGN KEY (user_id) REFERENCES app_user(user_id);

ALTER TABLE organization_membership
    ADD CONSTRAINT organization_membership_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE organization_membership
    ADD CONSTRAINT organization_membership_user_id_fkey FOREIGN KEY (user_id) REFERENCES app_user(user_id);

ALTER TABLE postmortem
    ADD CONSTRAINT postmortem_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES incident(org_id, incident_id);

ALTER TABLE relay_bootstrap_token
    ADD CONSTRAINT relay_bootstrap_token_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE relay_job
    ADD CONSTRAINT relay_job_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE relay_job
    ADD CONSTRAINT relay_job_investigation_belongs_to_organization FOREIGN KEY (org_id, investigation_id) REFERENCES investigation(org_id, investigation_id);

ALTER TABLE relay_job
    ADD CONSTRAINT relay_job_relay_is_in_the_same_org FOREIGN KEY (org_id, registration_id) REFERENCES relay_registration(org_id, registration_id);

ALTER TABLE relay_registration
    ADD CONSTRAINT relay_registration_organization_exists FOREIGN KEY (org_id) REFERENCES organization(org_id);

ALTER TABLE slack_conversation
    ADD CONSTRAINT slack_conversation_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE slack_conversation
    ADD CONSTRAINT slack_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES conversation(org_id, conversation_id);

ALTER TABLE slack_reply
    ADD CONSTRAINT slack_reply_names_its_conversation_investigation FOREIGN KEY (org_id, conversation_id, investigation_id) REFERENCES investigation(org_id, conversation_id, investigation_id);

ALTER TABLE slack_reply
    ADD CONSTRAINT slack_reply_names_its_mapping FOREIGN KEY (org_id, conversation_id) REFERENCES slack_conversation(org_id, conversation_id);

ALTER TABLE webhook_job
    ADD CONSTRAINT webhook_job_delivery_is_in_the_same_org FOREIGN KEY (org_id, delivery_id) REFERENCES webhook_delivery(org_id, delivery_id);

ALTER TABLE webhook_job
    ADD CONSTRAINT webhook_job_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES incident(org_id, incident_id);

ALTER TABLE webhook_job
    ADD CONSTRAINT webhook_job_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES integration(org_id, integration_id);

ALTER TABLE webhook_job
    ADD CONSTRAINT webhook_job_message_is_in_the_same_org FOREIGN KEY (org_id, conversation_id, message_sequence) REFERENCES conversation_message(org_id, conversation_id, sequence);
