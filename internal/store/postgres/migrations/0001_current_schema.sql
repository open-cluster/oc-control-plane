
CREATE FUNCTION public.audit_event_is_append_only() RETURNS trigger
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

CREATE FUNCTION public.prevent_slack_conversation_retargeting() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (NEW.org_id, NEW.conversation_id, NEW.integration_id, NEW.channel_id, NEW.thread_ts)
        IS DISTINCT FROM (OLD.org_id, OLD.conversation_id, OLD.integration_id, OLD.channel_id, OLD.thread_ts) THEN
        RAISE EXCEPTION 'Slack Conversation routing is immutable';
    END IF;
    RETURN NEW;
END $$;

SET default_tablespace = '';

SET default_table_access_method = heap;

CREATE TABLE public.alert_event (
    alert_event_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    source_key text NOT NULL,
    status smallint NOT NULL,
    title text NOT NULL,
    summary text NOT NULL,
    labels jsonb DEFAULT '{}'::jsonb NOT NULL,
    started_at timestamp with time zone NOT NULL,
    resolved_at timestamp with time zone,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    incident_id uuid,
    annotations jsonb DEFAULT '{}'::jsonb NOT NULL,
    generator_url text DEFAULT ''::text NOT NULL,
    CONSTRAINT alert_event_resolution_follows_its_start CHECK (((resolved_at IS NULL) OR (resolved_at >= started_at))),
    CONSTRAINT alert_event_resolution_is_stamped CHECK (((status = 2) = (resolved_at IS NOT NULL))),
    CONSTRAINT alert_event_status_check CHECK ((status = ANY (ARRAY[1, 2])))
);

CREATE TABLE public.app_user (
    user_id uuid DEFAULT gen_random_uuid() NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    email text NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    disabled_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.audit_event (
    event_id uuid NOT NULL,
    org_id uuid,
    actor_kind smallint NOT NULL,
    actor_id text DEFAULT ''::text NOT NULL,
    actor_display_name text DEFAULT ''::text NOT NULL,
    action text NOT NULL,
    target_kind text DEFAULT ''::text NOT NULL,
    target_id text DEFAULT ''::text NOT NULL,
    outcome smallint NOT NULL,
    source_address text DEFAULT ''::text NOT NULL,
    request_id text DEFAULT ''::text NOT NULL,
    detail jsonb DEFAULT '{}'::jsonb NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT audit_event_actor_kind_check CHECK ((actor_kind = ANY (ARRAY[1, 3]))),
    CONSTRAINT audit_event_deployment_action_check CHECK (((org_id IS NOT NULL) OR (action = ANY (ARRAY['session.signed-out'::text, 'session.revoked'::text, 'local.password-changed'::text, 'local.password-recovered'::text, 'local.bootstrap-completed'::text])))),
    CONSTRAINT audit_event_outcome_check CHECK ((outcome = ANY (ARRAY[1, 2, 3])))
);

CREATE TABLE public.change_event (
    change_event_id bigint NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    namespace text NOT NULL,
    object_kind smallint NOT NULL,
    object_name text NOT NULL,
    object_uid text NOT NULL,
    observed_revision text NOT NULL,
    change_kind smallint NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    fields jsonb DEFAULT '[]'::jsonb NOT NULL,
    CONSTRAINT change_event_change_kind_check CHECK ((change_kind = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT change_event_deletion_has_no_revision CHECK (((change_kind = 4) = (observed_revision = ''::text))),
    CONSTRAINT change_event_object_kind_check CHECK ((object_kind = ANY (ARRAY[1, 2, 3, 4, 5])))
);

ALTER TABLE public.change_event ALTER COLUMN change_event_id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.change_event_change_event_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.change_scope (
    integration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    policy_revision bigint DEFAULT 1 NOT NULL,
    requested_interval_seconds integer NOT NULL,
    covered_since timestamp with time zone,
    baseline_at timestamp with time zone,
    last_confirmed_at timestamp with time zone,
    faulted boolean DEFAULT false NOT NULL,
    truncated boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT change_scope_requested_interval_seconds_check CHECK ((requested_interval_seconds > 0))
);

CREATE TABLE public.conversation (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    incident_id uuid,
    surface smallint DEFAULT 1 NOT NULL,
    subject text NOT NULL,
    state smallint DEFAULT 1 NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_activity_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT conversation_state_check CHECK ((state = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_surface_check CHECK ((surface = ANY (ARRAY[1, 2])))
);

CREATE TABLE public.conversation_message (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    sequence bigint NOT NULL,
    role smallint NOT NULL,
    actor_kind smallint NOT NULL,
    actor_id text DEFAULT ''::text NOT NULL,
    actor_display text DEFAULT ''::text NOT NULL,
    text text NOT NULL,
    investigation_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_channel_id text DEFAULT ''::text NOT NULL,
    provider_message_id text DEFAULT ''::text NOT NULL,
    source_reference text DEFAULT ''::text NOT NULL,
    window_from timestamp with time zone NOT NULL,
    window_until timestamp with time zone NOT NULL,
    CONSTRAINT conversation_message_actor_kind_check CHECK ((actor_kind = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_message_role_check CHECK ((role = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_message_sequence_check CHECK ((sequence >= 1)),
    CONSTRAINT conversation_message_window_is_whole CHECK ((window_from < window_until))
);

CREATE TABLE public.incident (
    incident_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    grouping_key text NOT NULL,
    grouping_basis smallint NOT NULL,
    title text NOT NULL,
    status smallint NOT NULL,
    first_seen_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    resolved_at timestamp with time zone,
    superseded_by uuid,
    superseded_at timestamp with time zone,
    supersede_reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT incident_ends_after_it_starts CHECK ((last_seen_at >= first_seen_at)),
    CONSTRAINT incident_grouping_basis_check CHECK ((grouping_basis = ANY (ARRAY[1, 2]))),
    CONSTRAINT incident_resolution_is_stamped CHECK (((status = 2) = (resolved_at IS NOT NULL))),
    CONSTRAINT incident_status_check CHECK ((status = ANY (ARRAY[1, 2]))),
    CONSTRAINT incident_supersedes_something_else CHECK (((superseded_by IS NULL) OR (superseded_by <> incident_id))),
    CONSTRAINT incident_supersession_is_stamped CHECK (((superseded_by IS NULL) = (superseded_at IS NULL)))
);

CREATE TABLE public.integration (
    integration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    provider text NOT NULL,
    name text NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    relay_id uuid,
    credential_sealed bytea,
    webhook_secret_digest bytea,
    verification_status text,
    verified_at timestamp with time zone,
    verification_grants text[] DEFAULT '{}'::text[] NOT NULL,
    disabled boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT integration_verification_status_check CHECK (((verification_status IS NULL) OR (verification_status = ANY (ARRAY['verified'::text, 'failed'::text])))),
    CONSTRAINT integration_webhook_secret_digest_check CHECK (((webhook_secret_digest IS NULL) OR (length(webhook_secret_digest) = 32)))
);

CREATE TABLE public.integration_connect_flow (
    org_id uuid NOT NULL,
    provider text NOT NULL,
    principal text NOT NULL,
    state_digest bytea NOT NULL,
    return_to text DEFAULT '/'::text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT integration_connect_flow_state_digest_check CHECK ((length(state_digest) = 32))
);

CREATE TABLE public.integration_installation (
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    provider text NOT NULL,
    installation_key text[] NOT NULL,
    provider_actor_id text,
    CONSTRAINT integration_installation_key_is_complete CHECK (((cardinality(installation_key) > 0) AND (array_position(installation_key, ''::text) IS NULL) AND (array_position(installation_key, NULL::text) IS NULL)))
);

CREATE TABLE public.investigation (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    incident_id uuid,
    question text DEFAULT ''::text NOT NULL,
    subject text NOT NULL,
    window_from timestamp with time zone NOT NULL,
    window_until timestamp with time zone NOT NULL,
    status smallint DEFAULT 1 NOT NULL,
    conclusion jsonb DEFAULT '{}'::jsonb NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    spend_input_tokens bigint DEFAULT 0 NOT NULL,
    spend_output_tokens bigint DEFAULT 0 NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    concluded_at timestamp with time zone,
    stopped_by text DEFAULT ''::text NOT NULL,
    conversation_id uuid,
    turn smallint,
    lease_worker text DEFAULT ''::text NOT NULL,
    lease_expires_at timestamp with time zone,
    cancel_requested_at timestamp with time zone,
    cancelled_by text DEFAULT ''::text NOT NULL,
    lease_token uuid,
    automatic_incident boolean DEFAULT false NOT NULL,
    CONSTRAINT investigation_automatic_incident_has_no_conversation CHECK (((NOT automatic_incident) OR ((incident_id IS NOT NULL) AND (conversation_id IS NULL) AND (turn IS NULL)))),
    CONSTRAINT investigation_cancellation_is_attributed CHECK (((status = 4) = ((cancel_requested_at IS NOT NULL) AND (cancelled_by <> ''::text)))),
    CONSTRAINT investigation_conclusion_is_stamped CHECK (((status = 1) = (concluded_at IS NULL))),
    CONSTRAINT investigation_failure_states_a_reason CHECK (((status = 3) = (error <> ''::text))),
    CONSTRAINT investigation_lease_is_whole CHECK (((lease_worker = ''::text) = (lease_expires_at IS NULL))),
    CONSTRAINT investigation_lease_token_is_whole CHECK (((lease_worker = ''::text) = (lease_token IS NULL))),
    CONSTRAINT investigation_spend_input_tokens_check CHECK ((spend_input_tokens >= 0)),
    CONSTRAINT investigation_spend_output_tokens_check CHECK ((spend_output_tokens >= 0)),
    CONSTRAINT investigation_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT investigation_stop_is_a_conclusion CHECK (((stopped_by = ''::text) OR (status = 2))),
    CONSTRAINT investigation_turn_belongs_to_a_conversation CHECK (((conversation_id IS NULL) = (turn IS NULL))),
    CONSTRAINT investigation_turn_check CHECK ((turn >= 1)),
    CONSTRAINT investigation_window_ends_after_it_starts CHECK ((window_until >= window_from))
);

CREATE TABLE public.investigation_event (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    sequence bigint NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    type smallint NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT investigation_event_sequence_check CHECK ((sequence >= 1)),
    CONSTRAINT investigation_event_type_check CHECK (((type >= 1) AND (type <= 10)))
);

CREATE TABLE public.investigation_tool_run (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid,
    ordinal smallint NOT NULL,
    tool text NOT NULL,
    arguments jsonb DEFAULT '{}'::jsonb NOT NULL,
    window_from timestamp with time zone NOT NULL,
    window_until timestamp with time zone NOT NULL,
    outcome smallint NOT NULL,
    truncated boolean DEFAULT false NOT NULL,
    summary text DEFAULT ''::text NOT NULL,
    sources jsonb DEFAULT '[]'::jsonb NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone NOT NULL,
    purpose text DEFAULT ''::text NOT NULL,
    CONSTRAINT investigation_tool_run_failure_states_a_reason CHECK (((outcome = 2) = (error <> ''::text))),
    CONSTRAINT investigation_tool_run_ordinal_check CHECK ((ordinal >= 1)),
    CONSTRAINT investigation_tool_run_outcome_check CHECK ((outcome = ANY (ARRAY[1, 2])))
);

CREATE TABLE public.local_password (
    user_id uuid NOT NULL,
    password_hash text NOT NULL,
    changed_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT local_password_password_hash_check CHECK (((length(password_hash) >= 32) AND (length(password_hash) <= 512)))
);

CREATE TABLE public.oidc_sign_in_flow (
    state_digest bytea NOT NULL,
    code_verifier text,
    nonce text,
    return_to text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT oidc_sign_in_flow_state_digest_check CHECK ((octet_length(state_digest) = 32))
);

CREATE TABLE public.organization (
    org_id uuid DEFAULT gen_random_uuid() NOT NULL,
    display_name text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    audit_retention_days integer DEFAULT 90 NOT NULL,
    CONSTRAINT organization_audit_retention_days_check CHECK ((audit_retention_days >= 0))
);

CREATE TABLE public.organization_membership (
    org_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT organization_membership_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'editor'::text, 'viewer'::text])))
);

CREATE TABLE public.postmortem (
    incident_id uuid NOT NULL,
    org_id uuid NOT NULL,
    status text NOT NULL,
    revision integer NOT NULL,
    document jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    reviewed_at timestamp with time zone,
    reviewed_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT postmortem_review_is_stamped CHECK (((status = 'reviewed'::text) = (reviewed_at IS NOT NULL))),
    CONSTRAINT postmortem_revision_check CHECK ((revision >= 1)),
    CONSTRAINT postmortem_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'reviewed'::text])))
);

CREATE TABLE public.relay_bootstrap_token (
    bootstrap_digest bytea NOT NULL,
    org_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT relay_bootstrap_token_bootstrap_digest_check CHECK ((length(bootstrap_digest) = 32))
);

CREATE TABLE public.relay_job (
    job_id uuid NOT NULL,
    org_id uuid NOT NULL,
    registration_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    capability_id text NOT NULL,
    capability_version integer NOT NULL,
    arguments bytea NOT NULL,
    status smallint DEFAULT 0 NOT NULL,
    lease_session uuid,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamp with time zone,
    cancel_requested_at timestamp with time zone,
    result bytea,
    terminal_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    investigation_id uuid,
    CONSTRAINT relay_job_lease_is_whole CHECK ((((lease_session IS NULL) AND (lease_expires_at IS NULL)) OR ((lease_session IS NOT NULL) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT relay_job_status_check CHECK (((status >= 0) AND (status <= 4))),
    CONSTRAINT relay_job_terminal_is_stamped CHECK (((status = ANY (ARRAY[2, 3, 4])) = (terminal_at IS NOT NULL)))
);

CREATE TABLE public.relay_registration (
    registration_id uuid NOT NULL,
    org_id uuid NOT NULL,
    credential_digest bytea NOT NULL,
    cluster_fingerprint text NOT NULL,
    relay_version text NOT NULL,
    protocol_version bigint,
    capabilities jsonb NOT NULL,
    revoked_at timestamp with time zone,
    session_conflict_at timestamp with time zone,
    session_conflict_hosts integer DEFAULT 0 NOT NULL,
    session_id uuid,
    session_started_at timestamp with time zone,
    session_ended_at timestamp with time zone,
    last_seen_at timestamp with time zone,
    session_peer text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT relay_registration_credential_digest_check CHECK ((length(credential_digest) = 32)),
    CONSTRAINT relay_registration_protocol_version_check CHECK (((protocol_version IS NULL) OR ((protocol_version >= 1) AND (protocol_version <= '4294967295'::bigint))))
);

CREATE TABLE public.session (
    session_id uuid DEFAULT gen_random_uuid() NOT NULL,
    credential_digest bytea NOT NULL,
    user_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT session_credential_digest_check CHECK ((length(credential_digest) = 32))
);

CREATE TABLE public.slack_conversation (
    conversation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE public.slack_message_work (
    work_id uuid NOT NULL,
    org_id uuid NOT NULL,
    status smallint DEFAULT 1 NOT NULL,
    delivery_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    message_sequence bigint NOT NULL,
    attempts smallint DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    lease_owner text DEFAULT ''::text NOT NULL,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamp with time zone,
    failure_class text DEFAULT ''::text NOT NULL,
    failure_message text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    CONSTRAINT slack_message_work_attempts_check CHECK (((attempts >= 0) AND (attempts <= 12))),
    CONSTRAINT slack_message_work_failure_matches_retry_or_terminal CHECK ((((status = ANY (ARRAY[3, 4])) AND (failure_class <> ''::text)) OR ((status <> ALL (ARRAY[3, 4])) AND (failure_class = ''::text)))),
    CONSTRAINT slack_message_work_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT slack_message_work_lease_is_complete CHECK (((status = 2) = ((lease_owner <> ''::text) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT slack_message_work_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4, 5])))
);

CREATE TABLE public.slack_reply (
    investigation_id uuid NOT NULL,
    org_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    stream_ts text DEFAULT ''::text NOT NULL,
    native boolean DEFAULT false NOT NULL,
    status smallint DEFAULT 1 NOT NULL,
    last_sequence bigint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    note text DEFAULT ''::text NOT NULL,
    leased_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    lease_owner uuid,
    CONSTRAINT slack_reply_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT slack_reply_last_sequence_check CHECK ((last_sequence >= 0)),
    CONSTRAINT slack_reply_lease_is_whole CHECK (((leased_until IS NULL) = (lease_owner IS NULL))),
    CONSTRAINT slack_reply_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4])))
);

CREATE TABLE public.webhook_delivery (
    delivery_id uuid NOT NULL,
    org_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    content_digest bytea NOT NULL,
    truncated integer DEFAULT 0 NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_identity text NOT NULL,
    lifecycle_phase text DEFAULT ''::text NOT NULL,
    CONSTRAINT webhook_delivery_content_digest_check CHECK ((length(content_digest) = 32)),
    CONSTRAINT webhook_delivery_lifecycle_phase_check CHECK ((lifecycle_phase = ANY (ARRAY[''::text, 'firing'::text, 'resolved'::text]))),
    CONSTRAINT webhook_delivery_truncated_check CHECK ((truncated >= 0))
);

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_incident_is_unique UNIQUE (integration_id, source_key, started_at);

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_pkey PRIMARY KEY (alert_event_id);

ALTER TABLE ONLY public.app_user
    ADD CONSTRAINT app_user_identity_is_the_issuer_and_subject UNIQUE (issuer, subject);

ALTER TABLE ONLY public.app_user
    ADD CONSTRAINT app_user_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY public.audit_event
    ADD CONSTRAINT audit_event_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.change_event
    ADD CONSTRAINT change_event_is_unique_per_observation UNIQUE (integration_id, object_uid, observed_revision);

ALTER TABLE ONLY public.change_event
    ADD CONSTRAINT change_event_pkey PRIMARY KEY (change_event_id);

ALTER TABLE ONLY public.change_scope
    ADD CONSTRAINT change_scope_pkey PRIMARY KEY (integration_id);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id);

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_pkey PRIMARY KEY (org_id, conversation_id, sequence);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_identity_is_org_scoped UNIQUE (org_id, incident_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_pkey PRIMARY KEY (incident_id);

ALTER TABLE ONLY public.integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_pkey PRIMARY KEY (state_digest);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_identity_is_org_scoped UNIQUE (org_id, integration_id);

ALTER TABLE ONLY public.integration_installation
    ADD CONSTRAINT integration_installation_pkey PRIMARY KEY (org_id, integration_id);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_org_id_provider_unique UNIQUE (org_id, integration_id, provider);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_pkey PRIMARY KEY (integration_id);

ALTER TABLE ONLY public.investigation_event
    ADD CONSTRAINT investigation_event_pkey PRIMARY KEY (investigation_id, sequence);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_identity_in_conversation UNIQUE (org_id, conversation_id, investigation_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_identity_is_org_scoped UNIQUE (org_id, investigation_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_pkey PRIMARY KEY (investigation_id);

ALTER TABLE ONLY public.investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_pkey PRIMARY KEY (investigation_id, ordinal);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_turn_is_unique_in_its_conversation UNIQUE (org_id, conversation_id, turn);

ALTER TABLE ONLY public.local_password
    ADD CONSTRAINT local_password_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY public.oidc_sign_in_flow
    ADD CONSTRAINT oidc_sign_in_flow_pkey PRIMARY KEY (state_digest);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_pkey PRIMARY KEY (org_id, user_id);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_user_is_unique UNIQUE (user_id);

ALTER TABLE ONLY public.organization
    ADD CONSTRAINT organization_pkey PRIMARY KEY (org_id);

ALTER TABLE ONLY public.postmortem
    ADD CONSTRAINT postmortem_pkey PRIMARY KEY (org_id, incident_id);

ALTER TABLE ONLY public.relay_bootstrap_token
    ADD CONSTRAINT relay_bootstrap_token_pkey PRIMARY KEY (bootstrap_digest);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_pkey PRIMARY KEY (job_id);

ALTER TABLE ONLY public.relay_registration
    ADD CONSTRAINT relay_registration_identity_is_org_scoped UNIQUE (org_id, registration_id);

ALTER TABLE ONLY public.relay_registration
    ADD CONSTRAINT relay_registration_pkey PRIMARY KEY (registration_id);

ALTER TABLE ONLY public.session
    ADD CONSTRAINT session_credential_digest_is_unique UNIQUE (credential_digest);

ALTER TABLE ONLY public.session
    ADD CONSTRAINT session_pkey PRIMARY KEY (session_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_is_one_thread UNIQUE (integration_id, channel_id, thread_ts);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.slack_message_work
    ADD CONSTRAINT slack_message_work_identity_is_org_scoped UNIQUE (org_id, work_id);

ALTER TABLE ONLY public.slack_message_work
    ADD CONSTRAINT slack_message_work_pkey PRIMARY KEY (work_id);

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_pkey PRIMARY KEY (investigation_id);

ALTER TABLE ONLY public.webhook_delivery
    ADD CONSTRAINT webhook_delivery_identity_is_org_scoped UNIQUE (org_id, delivery_id);

ALTER TABLE ONLY public.webhook_delivery
    ADD CONSTRAINT webhook_delivery_pkey PRIMARY KEY (delivery_id);

CREATE INDEX alert_event_incident_idx ON public.alert_event USING btree (incident_id, started_at DESC);

CREATE INDEX alert_event_org_idx ON public.alert_event USING btree (org_id, received_at DESC);

CREATE INDEX alert_event_source_key_idx ON public.alert_event USING btree (integration_id, source_key, started_at DESC);

CREATE INDEX app_user_email_idx ON public.app_user USING btree (lower(email));

CREATE INDEX audit_event_actor_idx ON public.audit_event USING btree (org_id, actor_id, occurred_at DESC);

CREATE INDEX audit_event_org_idx ON public.audit_event USING btree (org_id, occurred_at DESC, event_id DESC);

CREATE INDEX audit_event_target_idx ON public.audit_event USING btree (org_id, target_kind, target_id, occurred_at DESC, event_id DESC);

CREATE INDEX change_event_retention_idx ON public.change_event USING btree (org_id, received_at);

CREATE INDEX change_event_window_idx ON public.change_event USING btree (integration_id, namespace, observed_at);

CREATE INDEX conversation_incident_idx ON public.conversation USING btree (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX conversation_message_assigned_idx ON public.conversation_message USING btree (org_id, investigation_id, conversation_id, sequence) WHERE ((role = 1) AND (investigation_id IS NOT NULL));

CREATE INDEX conversation_message_queued_idx ON public.conversation_message USING btree (org_id, conversation_id, sequence) WHERE (investigation_id IS NULL);

CREATE INDEX conversation_org_idx ON public.conversation USING btree (org_id, last_activity_at DESC, conversation_id DESC);

CREATE UNIQUE INDEX incident_open_key_idx ON public.incident USING btree (integration_id, grouping_key) WHERE (status = 1);

CREATE INDEX incident_org_idx ON public.incident USING btree (org_id, last_seen_at DESC, incident_id DESC);

CREATE INDEX integration_connect_flow_expiry_idx ON public.integration_connect_flow USING btree (expires_at);

CREATE UNIQUE INDEX integration_installation_provider_key_unique ON public.integration_installation USING btree (provider, installation_key);

CREATE INDEX integration_org_idx ON public.integration USING btree (org_id, created_at DESC);

CREATE INDEX integration_relay_idx ON public.integration USING btree (org_id, relay_id) WHERE (relay_id IS NOT NULL);

CREATE INDEX investigation_claimable_idx ON public.investigation USING btree (org_id, created_at, investigation_id) WHERE (status = 1);

CREATE INDEX investigation_incident_idx ON public.investigation USING btree (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX investigation_lease_expiry_idx ON public.investigation USING btree (lease_expires_at) WHERE ((status = 1) AND (lease_worker <> ''::text));

CREATE UNIQUE INDEX investigation_one_automatic_per_incident ON public.investigation USING btree (org_id, incident_id) WHERE automatic_incident;

CREATE UNIQUE INDEX investigation_one_running_per_conversation ON public.investigation USING btree (org_id, conversation_id) WHERE ((conversation_id IS NOT NULL) AND (status = 1));

CREATE INDEX investigation_org_idx ON public.investigation USING btree (org_id, created_at DESC, investigation_id DESC);

CREATE INDEX oidc_sign_in_flow_expiry ON public.oidc_sign_in_flow USING btree (expires_at);

CREATE INDEX organization_membership_org_idx ON public.organization_membership USING btree (org_id, created_at, user_id);

CREATE INDEX organization_membership_user_idx ON public.organization_membership USING btree (user_id);

CREATE INDEX relay_job_active_investigation_idx ON public.relay_job USING btree (org_id, investigation_id) WHERE ((investigation_id IS NOT NULL) AND (status = ANY (ARRAY[0, 1])));

CREATE INDEX relay_job_claimable_idx ON public.relay_job USING btree (org_id, registration_id, status, lease_expires_at) WHERE (status = ANY (ARRAY[0, 1]));

CREATE INDEX relay_registration_org_idx ON public.relay_registration USING btree (org_id, created_at DESC);

CREATE INDEX relay_registration_presence_idx ON public.relay_registration USING btree (org_id, last_seen_at DESC);

CREATE INDEX session_expiry_idx ON public.session USING btree (expires_at);

CREATE INDEX session_user_idx ON public.session USING btree (user_id);

CREATE INDEX slack_message_work_ready_idx ON public.slack_message_work USING btree (available_at, created_at, work_id) WHERE (status = ANY (ARRAY[1, 2, 3]));

CREATE UNIQUE INDEX slack_message_work_source_effect_is_unique ON public.slack_message_work USING btree (org_id, delivery_id, conversation_id, message_sequence);

CREATE INDEX slack_message_work_terminal_idx ON public.slack_message_work USING btree (org_id, updated_at DESC, work_id DESC) WHERE (status = 4);

CREATE INDEX slack_reply_due ON public.slack_reply USING btree (next_attempt_at, investigation_id) WHERE (status = ANY (ARRAY[1, 2]));

CREATE INDEX webhook_delivery_integration_idx ON public.webhook_delivery USING btree (org_id, integration_id, received_at DESC, delivery_id DESC);

CREATE UNIQUE INDEX webhook_delivery_provider_identity_is_unique ON public.webhook_delivery USING btree (integration_id, provider_identity, lifecycle_phase);

CREATE INDEX webhook_delivery_received_idx ON public.webhook_delivery USING btree (org_id, received_at DESC, delivery_id DESC);

CREATE TRIGGER audit_event_refuses_delete BEFORE DELETE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_truncate BEFORE TRUNCATE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_update BEFORE UPDATE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

CREATE TRIGGER slack_conversation_routing_is_immutable BEFORE UPDATE ON public.slack_conversation FOR EACH ROW EXECUTE FUNCTION public.prevent_slack_conversation_retargeting();

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_incident_id_fkey FOREIGN KEY (incident_id) REFERENCES public.incident(incident_id);

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.audit_event
    ADD CONSTRAINT audit_event_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.change_event
    ADD CONSTRAINT change_event_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.change_scope
    ADD CONSTRAINT change_scope_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_belongs_to_its_conversation FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id);

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_names_an_org_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES public.incident(incident_id);

ALTER TABLE ONLY public.integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.integration_installation
    ADD CONSTRAINT integration_installation_matches_parent_provider FOREIGN KEY (org_id, integration_id, provider) REFERENCES public.integration(org_id, integration_id, provider);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_relay_is_in_the_same_org FOREIGN KEY (org_id, relay_id) REFERENCES public.relay_registration(org_id, registration_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id);

ALTER TABLE ONLY public.investigation_event
    ADD CONSTRAINT investigation_event_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id);

ALTER TABLE ONLY public.investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_names_an_org_integration FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.local_password
    ADD CONSTRAINT local_password_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id);

ALTER TABLE ONLY public.postmortem
    ADD CONSTRAINT postmortem_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.relay_bootstrap_token
    ADD CONSTRAINT relay_bootstrap_token_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_investigation_belongs_to_organization FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_relay_is_in_the_same_org FOREIGN KEY (org_id, registration_id) REFERENCES public.relay_registration(org_id, registration_id);

ALTER TABLE ONLY public.relay_registration
    ADD CONSTRAINT relay_registration_organization_exists FOREIGN KEY (org_id) REFERENCES public.organization(org_id);

ALTER TABLE ONLY public.session
    ADD CONSTRAINT session_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id);

ALTER TABLE ONLY public.slack_message_work
    ADD CONSTRAINT slack_message_work_delivery_is_in_the_same_org FOREIGN KEY (org_id, delivery_id) REFERENCES public.webhook_delivery(org_id, delivery_id);

ALTER TABLE ONLY public.slack_message_work
    ADD CONSTRAINT slack_message_work_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.slack_message_work
    ADD CONSTRAINT slack_message_work_message_is_in_the_same_org FOREIGN KEY (org_id, conversation_id, message_sequence) REFERENCES public.conversation_message(org_id, conversation_id, sequence);

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_names_its_conversation_investigation FOREIGN KEY (org_id, conversation_id, investigation_id) REFERENCES public.investigation(org_id, conversation_id, investigation_id);

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_names_its_mapping FOREIGN KEY (org_id, conversation_id) REFERENCES public.slack_conversation(org_id, conversation_id);

ALTER TABLE ONLY public.webhook_delivery
    ADD CONSTRAINT webhook_delivery_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);
