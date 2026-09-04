SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

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

SET default_tablespace = '';

SET default_table_access_method = heap;

CREATE TABLE public.alert_event (
    alert_event_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_id uuid NOT NULL,
    source_key text NOT NULL,
    -- Status: 1 - firing, 2 - resolved
    status smallint NOT NULL,
    title text NOT NULL,
    summary text NOT NULL,
    labels jsonb DEFAULT '{}'::jsonb NOT NULL,
    started_at timestamp with time zone NOT NULL,
    resolved_at timestamp with time zone,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    incident_id uuid,
    annotations jsonb DEFAULT '{}'::jsonb NOT NULL,
    generator_url text DEFAULT ''::text NOT NULL,
    CONSTRAINT alert_event_generator_url_check CHECK ((length(generator_url) <= 2048)),
    CONSTRAINT alert_event_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT alert_event_resolution_follows_its_start CHECK (((resolved_at IS NULL) OR (resolved_at >= started_at))),
    CONSTRAINT alert_event_resolution_is_stamped CHECK (((status = 2) = (resolved_at IS NOT NULL))),
    CONSTRAINT alert_event_source_key_check CHECK (((length(source_key) >= 1) AND (length(source_key) <= 512))),
    CONSTRAINT alert_event_status_check CHECK ((status = ANY (ARRAY[1, 2]))),
    CONSTRAINT alert_event_summary_check CHECK ((length(summary) <= 4096)),
    CONSTRAINT alert_event_title_check CHECK ((length(title) <= 512))
);

CREATE TABLE public.app_user (
    user_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    email text NOT NULL,
    email_verified boolean DEFAULT false NOT NULL,
    display_name text DEFAULT ''::text NOT NULL,
    disabled_at timestamp with time zone,
    last_sign_in timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT app_user_display_name_check CHECK ((length(display_name) <= 256)),
    CONSTRAINT app_user_email_check CHECK (((length(email) >= 0) AND (length(email) <= 320))),
    CONSTRAINT app_user_issuer_check CHECK (((length(issuer) >= 1) AND (length(issuer) <= 512))),
    CONSTRAINT app_user_subject_check CHECK (((length(subject) >= 1) AND (length(subject) <= 256)))
);

CREATE TABLE public.audit_event (
    event_id uuid NOT NULL,
    org_id text NOT NULL,
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
    CONSTRAINT audit_event_action_check CHECK (((length(action) >= 1) AND (length(action) <= 128))),
    CONSTRAINT audit_event_actor_display_name_check CHECK ((length(actor_display_name) <= 256)),
    CONSTRAINT audit_event_actor_id_check CHECK ((length(actor_id) <= 256)),
    CONSTRAINT audit_event_actor_kind_check CHECK ((actor_kind = ANY (ARRAY[1, 3]))),
    CONSTRAINT audit_event_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT audit_event_outcome_check CHECK ((outcome = ANY (ARRAY[1, 2, 3]))),
    CONSTRAINT audit_event_request_id_check CHECK ((length(request_id) <= 128)),
    CONSTRAINT audit_event_source_address_check CHECK ((length(source_address) <= 128)),
    CONSTRAINT audit_event_target_id_check CHECK ((length(target_id) <= 256)),
    CONSTRAINT audit_event_target_kind_check CHECK ((length(target_kind) <= 64))
);

CREATE TABLE public.change_ledger (
    entry_id bigint NOT NULL,
    org_id text NOT NULL,
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
    CONSTRAINT change_ledger_change_kind_check CHECK ((change_kind = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT change_ledger_deletion_has_no_revision CHECK (((change_kind = 4) = (observed_revision = ''::text))),
    CONSTRAINT change_ledger_namespace_check CHECK (((length(namespace) >= 1) AND (length(namespace) <= 63))),
    CONSTRAINT change_ledger_object_kind_check CHECK ((object_kind = ANY (ARRAY[1, 2, 3, 4, 5]))),
    CONSTRAINT change_ledger_object_name_check CHECK (((length(object_name) >= 1) AND (length(object_name) <= 253))),
    CONSTRAINT change_ledger_object_uid_check CHECK (((length(object_uid) >= 1) AND (length(object_uid) <= 128))),
    CONSTRAINT change_ledger_observed_revision_check CHECK ((length(observed_revision) <= 128)),
    CONSTRAINT change_ledger_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128)))
);

ALTER TABLE public.change_ledger ALTER COLUMN entry_id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.change_ledger_entry_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.change_ledger_scope (
    integration_id uuid NOT NULL,
    org_id text NOT NULL,
    policy_revision bigint DEFAULT 1 NOT NULL,
    requested_interval_seconds integer NOT NULL,
    covered_since timestamp with time zone,
    baseline_at timestamp with time zone,
    last_confirmed_at timestamp with time zone,
    faulted boolean DEFAULT false NOT NULL,
    truncated boolean DEFAULT false NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT change_ledger_scope_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT change_ledger_scope_requested_interval_seconds_check CHECK ((requested_interval_seconds > 0))
);

CREATE TABLE public.conversation (
    conversation_id uuid NOT NULL,
    org_id text NOT NULL,
    incident_id uuid,
    surface smallint DEFAULT 1 NOT NULL,
    subject text NOT NULL,
    state smallint DEFAULT 1 NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_activity_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT conversation_created_by_check CHECK ((length(created_by) <= 256)),
    CONSTRAINT conversation_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT conversation_state_check CHECK ((state = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_subject_check CHECK (((length(subject) >= 1) AND (length(subject) <= 512))),
    CONSTRAINT conversation_surface_check CHECK ((surface = ANY (ARRAY[1, 2])))
);

CREATE TABLE public.conversation_message (
    conversation_id uuid NOT NULL,
    org_id text NOT NULL,
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
    CONSTRAINT conversation_message_actor_display_check CHECK ((length(actor_display) <= 256)),
    CONSTRAINT conversation_message_actor_id_check CHECK ((length(actor_id) <= 256)),
    CONSTRAINT conversation_message_actor_kind_check CHECK ((actor_kind = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_message_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT conversation_message_role_check CHECK ((role = ANY (ARRAY[1, 2]))),
    CONSTRAINT conversation_message_sequence_check CHECK ((sequence >= 1)),
    CONSTRAINT conversation_message_text_check CHECK (((length(text) >= 1) AND (length(text) <= 8192)))
);

CREATE TABLE public.deployment_sign_in_flow (
    flow_id uuid NOT NULL,
    org_id text NOT NULL,
    state_digest bytea NOT NULL,
    code_verifier text,
    nonce text,
    return_to text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT deployment_sign_in_flow_state_digest_check CHECK ((octet_length(state_digest) = 32))
);

CREATE TABLE public.incident (
    incident_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_id uuid NOT NULL,
    grouping_key text NOT NULL,
    grouping_basis smallint NOT NULL,
    title text NOT NULL,
    -- Status: 1 - open, 2 - resolved
    status smallint NOT NULL,
    first_seen_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone NOT NULL,
    resolved_at timestamp with time zone,
    alert_event_count integer DEFAULT 0 NOT NULL,
    superseded_by uuid,
    superseded_at timestamp with time zone,
    supersede_reason text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT incident_alert_event_count_check CHECK ((alert_event_count >= 0)),
    CONSTRAINT incident_ends_after_it_starts CHECK ((last_seen_at >= first_seen_at)),
    CONSTRAINT incident_grouping_basis_check CHECK ((grouping_basis = ANY (ARRAY[1, 2]))),
    CONSTRAINT incident_grouping_key_check CHECK (((length(grouping_key) >= 1) AND (length(grouping_key) <= 512))),
    CONSTRAINT incident_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT incident_resolution_is_stamped CHECK (((status = 2) = (resolved_at IS NOT NULL))),
    CONSTRAINT incident_status_check CHECK ((status = ANY (ARRAY[1, 2]))),
    CONSTRAINT incident_supersede_reason_check CHECK ((length(supersede_reason) <= 1024)),
    CONSTRAINT incident_supersedes_something_else CHECK (((superseded_by IS NULL) OR (superseded_by <> incident_id))),
    CONSTRAINT incident_supersession_is_stamped CHECK (((superseded_by IS NULL) = (superseded_at IS NULL))),
    CONSTRAINT incident_title_check CHECK ((length(title) <= 512))
);

CREATE TABLE public.integration (
    integration_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_type_id smallint NOT NULL,
    name text NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    webhook_secret_digest bytea,
    webhook_secret_fingerprint text,
    webhook_secret_created_at timestamp with time zone,
    webhook_secret_rotated_at timestamp with time zone,
    labels jsonb DEFAULT '{}'::jsonb NOT NULL,
    relay_id uuid,
    -- Status: 1 - configured, 2 - active, 3 - degraded, 4 - failed
    status smallint DEFAULT 1 NOT NULL,
    last_verified_at timestamp with time zone,
    verify_note text DEFAULT ''::text NOT NULL,
    disabled_at timestamp with time zone,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    credential_sealed bytea,
    credential_fingerprint text,
    credential_created_at timestamp with time zone,
    credential_rotated_at timestamp with time zone,
    verify_grants jsonb,
    verify_facts jsonb,
    credential_key_id text,
    CONSTRAINT integration_created_by_check CHECK ((length(created_by) <= 256)),
    CONSTRAINT integration_credential_fingerprint_check CHECK (((credential_fingerprint IS NULL) OR ((length(credential_fingerprint) >= 1) AND (length(credential_fingerprint) <= 64)))),
    CONSTRAINT integration_credential_is_whole CHECK ((((credential_sealed IS NULL) AND (credential_fingerprint IS NULL) AND (credential_created_at IS NULL)) OR ((credential_sealed IS NOT NULL) AND (credential_fingerprint IS NOT NULL) AND (credential_created_at IS NOT NULL)))),
    CONSTRAINT integration_name_check CHECK (((length(name) >= 1) AND (length(name) <= 128))),
    CONSTRAINT integration_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT integration_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT integration_verify_note_check CHECK ((length(verify_note) <= 512)),
    CONSTRAINT integration_webhook_secret_digest_check CHECK (((webhook_secret_digest IS NULL) OR (length(webhook_secret_digest) = 32))),
    CONSTRAINT integration_webhook_secret_fingerprint_check CHECK (((webhook_secret_fingerprint IS NULL) OR ((length(webhook_secret_fingerprint) >= 1) AND (length(webhook_secret_fingerprint) <= 64)))),
    CONSTRAINT integration_webhook_secret_is_whole CHECK ((((webhook_secret_digest IS NULL) AND (webhook_secret_fingerprint IS NULL) AND (webhook_secret_created_at IS NULL)) OR ((webhook_secret_digest IS NOT NULL) AND (webhook_secret_fingerprint IS NOT NULL) AND (webhook_secret_created_at IS NOT NULL))))
);

CREATE TABLE public.integration_connect_flow (
    flow_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_type_id smallint NOT NULL,
    principal text NOT NULL,
    state_digest bytea NOT NULL,
    return_to text DEFAULT '/'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    CONSTRAINT integration_connect_flow_expires_after_it_started CHECK ((expires_at > created_at)),
    CONSTRAINT integration_connect_flow_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT integration_connect_flow_principal_check CHECK (((length(principal) >= 1) AND (length(principal) <= 256))),
    CONSTRAINT integration_connect_flow_return_to_check CHECK ((length(return_to) <= 512)),
    CONSTRAINT integration_connect_flow_state_digest_check CHECK ((length(state_digest) = 32))
);

CREATE TABLE public.integration_delivery (
    delivery_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_id uuid NOT NULL,
    outcome smallint NOT NULL,
    body_digest bytea,
    reason text DEFAULT ''::text NOT NULL,
    alert_event_count integer DEFAULT 0 NOT NULL,
    truncated integer DEFAULT 0 NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    provider_identity text,
    lifecycle_phase text,
    request_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT integration_delivery_accepted_carries_a_digest CHECK (((outcome <> 1) OR (body_digest IS NOT NULL))),
    CONSTRAINT integration_delivery_alert_event_count_check CHECK ((alert_event_count >= 0)),
    CONSTRAINT integration_delivery_body_digest_check CHECK (((body_digest IS NULL) OR (length(body_digest) = 32))),
    CONSTRAINT integration_delivery_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT integration_delivery_outcome_check CHECK ((outcome = ANY (ARRAY[1, 2, 3]))),
    CONSTRAINT integration_delivery_reason_check CHECK ((length(reason) <= 64)),
    CONSTRAINT integration_delivery_states_a_reason_exactly_when_it_refused CHECK (((outcome = 3) = (reason <> ''::text))),
    CONSTRAINT integration_delivery_truncated_check CHECK ((truncated >= 0)),
    CONSTRAINT integration_delivery_accepted_carries_provider_identity CHECK (((outcome <> 1) OR ((provider_identity IS NOT NULL) AND (lifecycle_phase IS NOT NULL)))),
    CONSTRAINT integration_delivery_nonaccepted_has_no_provider_identity CHECK (((outcome = 1) OR ((provider_identity IS NULL) AND (lifecycle_phase IS NULL)))),
    CONSTRAINT integration_delivery_provider_identity_check CHECK (((provider_identity IS NULL) OR ((length(provider_identity) >= 1) AND (length(provider_identity) <= 256)))),
    CONSTRAINT integration_delivery_lifecycle_phase_check CHECK (((lifecycle_phase IS NULL) OR (lifecycle_phase = ANY (ARRAY[''::text, 'firing'::text, 'resolved'::text])))),
    CONSTRAINT integration_delivery_request_id_check CHECK ((length(request_id) <= 128))
);

CREATE TABLE public.integration_installation (
    integration_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_type_id smallint NOT NULL,
    application text NOT NULL,
    enterprise text DEFAULT ''::text NOT NULL,
    workspace text NOT NULL,
    enterprise_wide boolean DEFAULT false NOT NULL,
    agent text DEFAULT ''::text NOT NULL,
    authorizer text DEFAULT ''::text NOT NULL,
    grants text[] DEFAULT '{}'::text[] NOT NULL,
    expires_at timestamp with time zone,
    refresh_sealed bytea,
    installed_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT integration_installation_agent_check CHECK ((length(agent) <= 64)),
    CONSTRAINT integration_installation_application_check CHECK (((length(application) >= 1) AND (length(application) <= 64))),
    CONSTRAINT integration_installation_authorizer_check CHECK ((length(authorizer) <= 64)),
    CONSTRAINT integration_installation_enterprise_check CHECK ((length(enterprise) <= 64)),
    CONSTRAINT integration_installation_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT integration_installation_workspace_check CHECK (((length(workspace) >= 1) AND (length(workspace) <= 64)))
);

CREATE TABLE public.integration_type (
    integration_type_id smallint NOT NULL,
    key text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    logo text DEFAULT ''::text NOT NULL,
    category text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT integration_type_category_check CHECK (((length(category) >= 1) AND (length(category) <= 64))),
    CONSTRAINT integration_type_description_check CHECK ((length(description) <= 512)),
    CONSTRAINT integration_type_key_check CHECK (((length(key) >= 1) AND (length(key) <= 64))),
    CONSTRAINT integration_type_logo_check CHECK ((length(logo) <= 64)),
    CONSTRAINT integration_type_name_check CHECK (((length(name) >= 1) AND (length(name) <= 128)))
);

CREATE TABLE public.investigation (
    investigation_id uuid NOT NULL,
    org_id text NOT NULL,
    incident_id uuid,
    question text DEFAULT ''::text NOT NULL,
    subject text NOT NULL,
    window_from timestamp with time zone NOT NULL,
    window_until timestamp with time zone NOT NULL,
    -- Status: 1 - running, 2 - concluded, 3 - failed, 4 - cancelled
    status smallint DEFAULT 1 NOT NULL,
    conclusion jsonb DEFAULT '{}'::jsonb NOT NULL,
    error text DEFAULT ''::text NOT NULL,
    spend_input_tokens bigint DEFAULT 0 NOT NULL,
    spend_output_tokens bigint DEFAULT 0 NOT NULL,
    spend_micro_cents bigint DEFAULT 0 NOT NULL,
    created_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    concluded_at timestamp with time zone,
    stopped_by text DEFAULT ''::text NOT NULL,
    conversation_id uuid,
    turn smallint,
    lease_worker text DEFAULT ''::text NOT NULL,
    lease_expires_at timestamp with time zone,
    webhook_work_id uuid,
    cancel_requested_at timestamp with time zone,
    cancelled_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT investigation_cancellation_is_attributed CHECK (((status = 4) = ((cancel_requested_at IS NOT NULL) AND (cancelled_by <> ''::text)))),
    CONSTRAINT investigation_cancelled_by_check CHECK ((length(cancelled_by) <= 256)),
    CONSTRAINT investigation_conclusion_is_stamped CHECK (((status = 1) = (concluded_at IS NULL))),
    CONSTRAINT investigation_created_by_check CHECK ((length(created_by) <= 256)),
    CONSTRAINT investigation_error_check CHECK ((length(error) <= 1024)),
    CONSTRAINT investigation_failure_states_a_reason CHECK (((status = 3) = (error <> ''::text))),
    CONSTRAINT investigation_lease_is_whole CHECK (((lease_worker = ''::text) = (lease_expires_at IS NULL))),
    CONSTRAINT investigation_lease_worker_check CHECK ((length(lease_worker) <= 128)),
    CONSTRAINT investigation_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT investigation_question_check CHECK ((length(question) <= 1024)),
    CONSTRAINT investigation_spend_input_tokens_check CHECK ((spend_input_tokens >= 0)),
    CONSTRAINT investigation_spend_micro_cents_check CHECK ((spend_micro_cents >= 0)),
    CONSTRAINT investigation_spend_output_tokens_check CHECK ((spend_output_tokens >= 0)),
    CONSTRAINT investigation_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT investigation_stop_is_a_conclusion CHECK (((stopped_by = ''::text) OR (status = 2))),
    CONSTRAINT investigation_stopped_by_check CHECK ((length(stopped_by) <= 64)),
    CONSTRAINT investigation_subject_check CHECK (((length(subject) >= 1) AND (length(subject) <= 512))),
    CONSTRAINT investigation_turn_belongs_to_a_conversation CHECK (((conversation_id IS NULL) = (turn IS NULL))),
    CONSTRAINT investigation_turn_check CHECK ((turn >= 1)),
    CONSTRAINT investigation_window_ends_after_it_starts CHECK ((window_until >= window_from))
);

CREATE TABLE public.investigation_event (
    investigation_id uuid NOT NULL,
    org_id text NOT NULL,
    sequence bigint NOT NULL,
    at timestamp with time zone DEFAULT now() NOT NULL,
    -- Type:
        -- 1 - started,
        -- 2 - progress,
        -- 3 - tool_started,
        -- 4 - tool_completed,
        -- 5 - retired,
        -- 6 - concluded,
        -- 7 - failed,
        -- 8 - retired,
        -- 9 - canceled,
        -- 10 - hypotheses_updated
    type smallint NOT NULL,
    payload jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT investigation_event_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT investigation_event_sequence_check CHECK ((sequence >= 1)),
    CONSTRAINT investigation_event_type_check CHECK (((type >= 1) AND (type <= 10)))
);

CREATE TABLE public.investigation_tool_run (
    investigation_id uuid NOT NULL,
    org_id text NOT NULL,
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
    hypothesis_id text DEFAULT ''::text NOT NULL,
    CONSTRAINT investigation_tool_run_error_check CHECK ((length(error) <= 1024)),
    CONSTRAINT investigation_tool_run_failure_states_a_reason CHECK (((outcome = 2) = (error <> ''::text))),
    CONSTRAINT investigation_tool_run_ordinal_check CHECK ((ordinal >= 1)),
    CONSTRAINT investigation_tool_run_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT investigation_tool_run_outcome_check CHECK ((outcome = ANY (ARRAY[1, 2]))),
    CONSTRAINT investigation_tool_run_summary_check CHECK ((length(summary) <= 512)),
    CONSTRAINT investigation_tool_run_tool_check CHECK (((length(tool) >= 1) AND (length(tool) <= 128)))
);

CREATE TABLE public.local_password (
    user_id uuid NOT NULL,
    password_hash text NOT NULL,
    password_changed_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT local_password_password_hash_check CHECK (((length(password_hash) >= 32) AND (length(password_hash) <= 512)))
);

CREATE TABLE public.operator_session (
    session_id uuid NOT NULL,
    token_digest bytea NOT NULL,
    user_id uuid NOT NULL,
    org_id text,
    issued_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revoked_by text DEFAULT ''::text NOT NULL,
    user_agent text DEFAULT ''::text NOT NULL,
    address text DEFAULT ''::text NOT NULL,
    CONSTRAINT operator_session_address_check CHECK ((length(address) <= 128)),
    CONSTRAINT operator_session_expires_after_it_was_issued CHECK ((expires_at > issued_at)),
    CONSTRAINT operator_session_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT operator_session_revoked_by_check CHECK ((length(revoked_by) <= 256)),
    CONSTRAINT operator_session_token_digest_check CHECK ((length(token_digest) = 32)),
    CONSTRAINT operator_session_user_agent_check CHECK ((length(user_agent) <= 256))
);

CREATE TABLE public.organization (
    org_id text NOT NULL,
    display_name text NOT NULL,
    created_by text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT organization_pkey PRIMARY KEY (org_id),
    CONSTRAINT organization_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT organization_display_name_check CHECK (((length(display_name) >= 1) AND (length(display_name) <= 256))),
    CONSTRAINT organization_created_by_check CHECK (((length(created_by) >= 1) AND (length(created_by) <= 256)))
);

CREATE TABLE public.organization_membership (
    membership_id uuid NOT NULL,
    org_id text NOT NULL,
    user_id uuid NOT NULL,
    role text,
    source smallint NOT NULL,
    external_id text,
    active boolean DEFAULT true NOT NULL,
    granted_by text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT organization_membership_external_id_check CHECK (((external_id IS NULL) OR ((length(external_id) >= 1) AND (length(external_id) <= 256)))),
    CONSTRAINT organization_membership_granted_by_check CHECK ((length(granted_by) <= 256)),
    CONSTRAINT organization_membership_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT organization_membership_role_check CHECK (((role IS NULL) OR ((length(role) >= 1) AND (length(role) <= 64)))),
    CONSTRAINT organization_membership_source_check CHECK ((source = ANY (ARRAY[1, 2, 3])))
);

CREATE TABLE public.organization_policy (
    org_id text NOT NULL,
    session_lifetime_seconds integer DEFAULT 0 NOT NULL,
    audit_retention_days integer DEFAULT 0 NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT organization_policy_audit_retention_days_check CHECK ((audit_retention_days >= 0)),
    CONSTRAINT organization_policy_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT organization_policy_session_lifetime_seconds_check CHECK ((session_lifetime_seconds >= 0)),
    CONSTRAINT organization_policy_updated_by_check CHECK ((length(updated_by) <= 256))
);

CREATE TABLE public.postmortem (
    incident_id uuid NOT NULL,
    org_id text NOT NULL,
    status text NOT NULL,
    revision integer NOT NULL,
    document jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    reviewed_at timestamp with time zone,
    reviewed_by text DEFAULT ''::text NOT NULL,
    CONSTRAINT postmortem_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT postmortem_review_is_stamped CHECK (((status = 'reviewed'::text) = (reviewed_at IS NOT NULL))),
    CONSTRAINT postmortem_reviewed_by_check CHECK ((length(reviewed_by) <= 256)),
    CONSTRAINT postmortem_revision_check CHECK ((revision >= 1)),
    CONSTRAINT postmortem_status_check CHECK ((status = ANY (ARRAY['draft'::text, 'reviewed'::text])))
);

CREATE TABLE public.relay_bootstrap_token (
    token_digest bytea NOT NULL,
    org_id text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT relay_bootstrap_token_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT relay_bootstrap_token_token_digest_check CHECK ((length(token_digest) = 32))
);

CREATE TABLE public.relay_job (
    job_id uuid NOT NULL,
    org_id text NOT NULL,
    registration_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    capability_id text NOT NULL,
    capability_version integer NOT NULL,
    arguments bytea NOT NULL,
    -- Status: 0 - pending, 1 - leased, 2 - succeeded, 3 - failed, 4 - cancelled
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
    CONSTRAINT relay_job_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT relay_job_status_check CHECK (((status >= 0) AND (status <= 4))),
    CONSTRAINT relay_job_terminal_is_stamped CHECK (((status = ANY (ARRAY[2, 3, 4])) = (terminal_at IS NOT NULL)))
);

CREATE TABLE public.relay_registration (
    registration_id uuid NOT NULL,
    org_id text NOT NULL,
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
    CONSTRAINT relay_registration_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT relay_registration_session_peer_check CHECK (((session_peer IS NULL) OR (length(session_peer) <= 256))),
    CONSTRAINT relay_registration_protocol_version_check CHECK (((protocol_version IS NULL) OR ((protocol_version >= 1) AND (protocol_version <= 4294967295))))
);

CREATE TABLE public.relay_session_conflict_event (
    event_id bigint NOT NULL,
    org_id text NOT NULL,
    registration_id uuid NOT NULL,
    kind smallint NOT NULL,
    distinct_hosts integer DEFAULT 0 NOT NULL,
    withdrawn_from text,
    at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT relay_session_conflict_event_actor_belongs_to_a_withdrawal CHECK (((kind = 2) OR (withdrawn_from IS NULL))),
    CONSTRAINT relay_session_conflict_event_distinct_hosts_check CHECK ((distinct_hosts >= 0)),
    CONSTRAINT relay_session_conflict_event_kind_check CHECK ((kind = ANY (ARRAY[1, 2]))),
    CONSTRAINT relay_session_conflict_event_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128)))
);

ALTER TABLE public.relay_session_conflict_event ALTER COLUMN event_id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.relay_session_conflict_event_event_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE public.slack_conversation (
    conversation_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_id uuid NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT slack_conversation_channel_id_check CHECK (((length(channel_id) >= 1) AND (length(channel_id) <= 64))),
    CONSTRAINT slack_conversation_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT slack_conversation_thread_ts_check CHECK (((length(thread_ts) >= 1) AND (length(thread_ts) <= 64)))
);

CREATE TABLE public.slack_reply (
    investigation_id uuid NOT NULL,
    org_id text NOT NULL,
    integration_id uuid NOT NULL,
    conversation_id uuid NOT NULL,
    channel_id text NOT NULL,
    thread_ts text NOT NULL,
    stream_ts text DEFAULT ''::text NOT NULL,
    native boolean DEFAULT false NOT NULL,
    -- Status: 1 - pending, 2 - delivering, 3 - delivered, 4 - failed
    status smallint DEFAULT 1 NOT NULL,
    last_sequence bigint DEFAULT 0 NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    note text DEFAULT ''::text NOT NULL,
    leased_until timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT slack_reply_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT slack_reply_channel_id_check CHECK (((length(channel_id) >= 1) AND (length(channel_id) <= 64))),
    CONSTRAINT slack_reply_last_sequence_check CHECK ((last_sequence >= 0)),
    CONSTRAINT slack_reply_note_check CHECK ((length(note) <= 512)),
    CONSTRAINT slack_reply_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT slack_reply_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4]))),
    CONSTRAINT slack_reply_stream_ts_check CHECK ((length(stream_ts) <= 64)),
    CONSTRAINT slack_reply_thread_ts_check CHECK (((length(thread_ts) >= 1) AND (length(thread_ts) <= 64)))
);

CREATE TABLE public.webhook_work (
    work_id uuid NOT NULL,
    org_id text NOT NULL,
    kind smallint NOT NULL,
    -- Status: 1 - ready, 2 - leased, 3 - retry, 4 - terminal, 5 - complete
    status smallint DEFAULT 1 NOT NULL,
    delivery_id uuid NOT NULL,
    integration_id uuid NOT NULL,
    incident_id uuid,
    conversation_id uuid,
    message_sequence bigint,
    attempts smallint DEFAULT 0 NOT NULL,
    available_at timestamp with time zone DEFAULT now() NOT NULL,
    lease_owner text DEFAULT ''::text NOT NULL,
    lease_epoch bigint DEFAULT 0 NOT NULL,
    lease_expires_at timestamp with time zone,
    failure_class text DEFAULT ''::text NOT NULL,
    failure_message text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT webhook_work_attempts_check CHECK (((attempts >= 0) AND (attempts <= 12))),
    CONSTRAINT webhook_work_failure_class_check CHECK ((length(failure_class) <= 64)),
    CONSTRAINT webhook_work_failure_matches_retry_or_terminal CHECK ((((status = ANY (ARRAY[3, 4])) AND (failure_class <> ''::text)) OR ((status <> ALL (ARRAY[3, 4])) AND (failure_class = ''::text)))),
    CONSTRAINT webhook_work_failure_message_check CHECK ((length(failure_message) <= 512)),
    CONSTRAINT webhook_work_has_one_effect_reference CHECK ((((kind = 1) AND (incident_id IS NOT NULL) AND (conversation_id IS NULL) AND (message_sequence IS NULL)) OR ((kind = 2) AND (incident_id IS NULL) AND (conversation_id IS NOT NULL) AND (message_sequence IS NOT NULL)))),
    CONSTRAINT webhook_work_kind_check CHECK ((kind = ANY (ARRAY[1, 2]))),
    CONSTRAINT webhook_work_lease_epoch_check CHECK ((lease_epoch >= 0)),
    CONSTRAINT webhook_work_lease_is_complete CHECK (((status = 2) = ((lease_owner <> ''::text) AND (lease_expires_at IS NOT NULL)))),
    CONSTRAINT webhook_work_lease_owner_check CHECK ((length(lease_owner) <= 128)),
    CONSTRAINT webhook_work_org_id_check CHECK (((length(org_id) >= 1) AND (length(org_id) <= 128))),
    CONSTRAINT webhook_work_status_check CHECK ((status = ANY (ARRAY[1, 2, 3, 4, 5])))
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

ALTER TABLE ONLY public.change_ledger
    ADD CONSTRAINT change_ledger_entry_is_unique_per_observation UNIQUE (integration_id, object_uid, observed_revision);

ALTER TABLE ONLY public.change_ledger
    ADD CONSTRAINT change_ledger_pkey PRIMARY KEY (entry_id);

ALTER TABLE ONLY public.change_ledger_scope
    ADD CONSTRAINT change_ledger_scope_pkey PRIMARY KEY (integration_id);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_identity_is_org_scoped UNIQUE (org_id, conversation_id);

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_pkey PRIMARY KEY (org_id, conversation_id, sequence);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.deployment_sign_in_flow
    ADD CONSTRAINT deployment_sign_in_flow_pkey PRIMARY KEY (flow_id);

ALTER TABLE ONLY public.deployment_sign_in_flow
    ADD CONSTRAINT deployment_sign_in_flow_state_digest_key UNIQUE (state_digest);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_identity_is_org_scoped UNIQUE (org_id, incident_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_pkey PRIMARY KEY (incident_id);

ALTER TABLE ONLY public.integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_pkey PRIMARY KEY (flow_id);

ALTER TABLE ONLY public.integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_state_is_unique UNIQUE (state_digest);

ALTER TABLE ONLY public.integration_delivery
    ADD CONSTRAINT integration_delivery_identity_is_org_scoped UNIQUE (org_id, delivery_id);

ALTER TABLE ONLY public.integration_delivery
    ADD CONSTRAINT integration_delivery_pkey PRIMARY KEY (delivery_id);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_identity_is_org_scoped UNIQUE (org_id, integration_id);

ALTER TABLE ONLY public.integration_installation
    ADD CONSTRAINT integration_installation_pkey PRIMARY KEY (integration_id);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_name_is_unique_per_org UNIQUE (org_id, name);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_pkey PRIMARY KEY (integration_id);

ALTER TABLE ONLY public.integration_type
    ADD CONSTRAINT integration_type_key_key UNIQUE (key);

ALTER TABLE ONLY public.integration_type
    ADD CONSTRAINT integration_type_pkey PRIMARY KEY (integration_type_id);

ALTER TABLE ONLY public.investigation_event
    ADD CONSTRAINT investigation_event_pkey PRIMARY KEY (investigation_id, sequence);

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

ALTER TABLE ONLY public.operator_session
    ADD CONSTRAINT operator_session_digest_is_unique UNIQUE (token_digest);

ALTER TABLE ONLY public.operator_session
    ADD CONSTRAINT operator_session_pkey PRIMARY KEY (session_id);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_is_one_per_tenant UNIQUE (org_id, user_id);

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_pkey PRIMARY KEY (membership_id);

ALTER TABLE ONLY public.organization_policy
    ADD CONSTRAINT organization_policy_pkey PRIMARY KEY (org_id);

ALTER TABLE ONLY public.postmortem
    ADD CONSTRAINT postmortem_pkey PRIMARY KEY (org_id, incident_id);

ALTER TABLE ONLY public.relay_bootstrap_token
    ADD CONSTRAINT relay_bootstrap_token_pkey PRIMARY KEY (token_digest);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_pkey PRIMARY KEY (job_id);

ALTER TABLE ONLY public.relay_registration
    ADD CONSTRAINT relay_registration_identity_is_org_scoped UNIQUE (org_id, registration_id);

ALTER TABLE ONLY public.relay_registration
    ADD CONSTRAINT relay_registration_pkey PRIMARY KEY (registration_id);

ALTER TABLE ONLY public.relay_session_conflict_event
    ADD CONSTRAINT relay_session_conflict_event_pkey PRIMARY KEY (event_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_is_one_thread UNIQUE (integration_id, channel_id, thread_ts);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_pkey PRIMARY KEY (conversation_id);

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_pkey PRIMARY KEY (investigation_id);

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_identity_is_org_scoped UNIQUE (org_id, work_id);

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_pkey PRIMARY KEY (work_id);

CREATE INDEX alert_event_incident_idx ON public.alert_event USING btree (incident_id, started_at DESC);

CREATE INDEX alert_event_org_idx ON public.alert_event USING btree (org_id, received_at DESC);

CREATE INDEX alert_event_source_key_idx ON public.alert_event USING btree (integration_id, source_key, started_at DESC);

CREATE INDEX app_user_email_idx ON public.app_user USING btree (lower(email));

CREATE INDEX audit_event_actor_idx ON public.audit_event USING btree (org_id, actor_id, occurred_at DESC);

CREATE INDEX audit_event_org_idx ON public.audit_event USING btree (org_id, occurred_at DESC, event_id DESC);

CREATE INDEX audit_event_target_idx ON public.audit_event USING btree (org_id, target_kind, target_id, occurred_at DESC);

CREATE INDEX change_ledger_retention_idx ON public.change_ledger USING btree (org_id, received_at);

CREATE INDEX change_ledger_window_idx ON public.change_ledger USING btree (integration_id, namespace, observed_at);

CREATE INDEX conversation_incident_idx ON public.conversation USING btree (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX conversation_message_queued_idx ON public.conversation_message USING btree (org_id, conversation_id, sequence) WHERE (investigation_id IS NULL);

CREATE INDEX conversation_message_person_history_idx ON public.conversation_message USING btree (org_id, conversation_id, sequence) WHERE (role = 1);

CREATE INDEX conversation_org_idx ON public.conversation USING btree (org_id, last_activity_at DESC, conversation_id DESC);

CREATE INDEX deployment_sign_in_flow_expiry ON public.deployment_sign_in_flow USING btree (expires_at);

CREATE UNIQUE INDEX incident_open_key_idx ON public.incident USING btree (integration_id, grouping_key) WHERE (status = 1);

CREATE INDEX incident_org_idx ON public.incident USING btree (org_id, last_seen_at DESC, incident_id DESC);

CREATE INDEX integration_connect_flow_expiry_idx ON public.integration_connect_flow USING btree (expires_at);

CREATE INDEX integration_delivery_accepted_idx ON public.integration_delivery USING btree (integration_id, received_at DESC) WHERE (outcome = 1);

CREATE UNIQUE INDEX integration_delivery_accepted_provider_identity_is_unique ON public.integration_delivery USING btree (integration_id, provider_identity, lifecycle_phase) WHERE (outcome = 1);

CREATE INDEX integration_delivery_integration_idx ON public.integration_delivery USING btree (org_id, integration_id, received_at DESC, delivery_id DESC);

CREATE UNIQUE INDEX integration_installation_is_one_workspace ON public.integration_installation USING btree (integration_type_id, application, enterprise, workspace);

CREATE INDEX integration_org_idx ON public.integration USING btree (org_id, created_at DESC);

CREATE INDEX integration_relay_idx ON public.integration USING btree (org_id, relay_id) WHERE (relay_id IS NOT NULL);

CREATE INDEX investigation_claimable_idx ON public.investigation USING btree (org_id, created_at, investigation_id) WHERE (status = 1);

CREATE INDEX investigation_incident_idx ON public.investigation USING btree (incident_id) WHERE (incident_id IS NOT NULL);

CREATE INDEX investigation_lease_expiry_idx ON public.investigation USING btree (lease_expires_at) WHERE ((status = 1) AND (lease_worker <> ''::text));

CREATE UNIQUE INDEX investigation_one_running_per_conversation ON public.investigation USING btree (org_id, conversation_id) WHERE ((conversation_id IS NOT NULL) AND (status = 1));

CREATE INDEX investigation_org_idx ON public.investigation USING btree (org_id, created_at DESC, investigation_id DESC);

CREATE UNIQUE INDEX investigation_webhook_work_is_unique ON public.investigation USING btree (org_id, webhook_work_id) WHERE (webhook_work_id IS NOT NULL);

CREATE INDEX operator_session_expiry_idx ON public.operator_session USING btree (expires_at);

CREATE INDEX operator_session_user_idx ON public.operator_session USING btree (user_id, issued_at DESC);

CREATE UNIQUE INDEX organization_membership_external_id_is_unique_per_org ON public.organization_membership USING btree (org_id, external_id) WHERE (external_id IS NOT NULL);

CREATE INDEX organization_membership_org_idx ON public.organization_membership USING btree (org_id, created_at DESC);

CREATE INDEX organization_membership_user_idx ON public.organization_membership USING btree (user_id);

CREATE INDEX relay_job_active_investigation_idx ON public.relay_job USING btree (org_id, investigation_id) WHERE ((investigation_id IS NOT NULL) AND (status = ANY (ARRAY[0, 1])));

CREATE INDEX relay_job_claimable_idx ON public.relay_job USING btree (org_id, registration_id, status, lease_expires_at) WHERE (status = ANY (ARRAY[0, 1]));

CREATE INDEX relay_registration_org_idx ON public.relay_registration USING btree (org_id, created_at DESC);

CREATE INDEX relay_registration_presence_idx ON public.relay_registration USING btree (org_id, last_seen_at DESC);

CREATE INDEX relay_session_conflict_event_registration_idx ON public.relay_session_conflict_event USING btree (org_id, registration_id, event_id DESC);

CREATE INDEX slack_conversation_by_thread ON public.slack_conversation USING btree (integration_id, channel_id, thread_ts);

CREATE INDEX slack_reply_due ON public.slack_reply USING btree (next_attempt_at, investigation_id) WHERE (status = ANY (ARRAY[1, 2]));

CREATE INDEX webhook_work_ready_idx ON public.webhook_work USING btree (available_at, created_at, work_id) WHERE (status = ANY (ARRAY[1, 2, 3]));

CREATE UNIQUE INDEX webhook_work_source_effect_is_unique ON public.webhook_work USING btree (org_id, kind, delivery_id, COALESCE(incident_id, conversation_id), COALESCE(message_sequence, (0)::bigint));

CREATE INDEX webhook_work_terminal_idx ON public.webhook_work USING btree (org_id, updated_at DESC, work_id DESC) WHERE (status = 4);

CREATE TRIGGER audit_event_refuses_delete BEFORE DELETE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_truncate BEFORE TRUNCATE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

CREATE TRIGGER audit_event_refuses_update BEFORE UPDATE ON public.audit_event FOR EACH STATEMENT EXECUTE FUNCTION public.audit_event_is_append_only();

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_incident_id_fkey FOREIGN KEY (incident_id) REFERENCES public.incident(incident_id);

ALTER TABLE ONLY public.alert_event
    ADD CONSTRAINT alert_event_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.change_ledger
    ADD CONSTRAINT change_ledger_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.change_ledger_scope
    ADD CONSTRAINT change_ledger_scope_integration_is_in_the_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.conversation
    ADD CONSTRAINT conversation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_belongs_to_its_conversation FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.conversation_message
    ADD CONSTRAINT conversation_message_names_an_org_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.incident
    ADD CONSTRAINT incident_superseded_by_fkey FOREIGN KEY (superseded_by) REFERENCES public.incident(incident_id);

ALTER TABLE ONLY public.integration_connect_flow
    ADD CONSTRAINT integration_connect_flow_integration_type_id_fkey FOREIGN KEY (integration_type_id) REFERENCES public.integration_type(integration_type_id);

ALTER TABLE ONLY public.integration_delivery
    ADD CONSTRAINT integration_delivery_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.integration_installation
    ADD CONSTRAINT integration_installation_integration_type_id_fkey FOREIGN KEY (integration_type_id) REFERENCES public.integration_type(integration_type_id);

ALTER TABLE ONLY public.integration_installation
    ADD CONSTRAINT integration_installation_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_integration_type_id_fkey FOREIGN KEY (integration_type_id) REFERENCES public.integration_type(integration_type_id);

ALTER TABLE ONLY public.integration
    ADD CONSTRAINT integration_relay_is_in_the_same_org FOREIGN KEY (org_id, relay_id) REFERENCES public.relay_registration(org_id, registration_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id);

ALTER TABLE ONLY public.investigation_event
    ADD CONSTRAINT investigation_event_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_belongs_to_its_investigation FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.investigation_tool_run
    ADD CONSTRAINT investigation_tool_run_names_an_org_integration FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.investigation
    ADD CONSTRAINT investigation_webhook_work_is_in_the_same_org FOREIGN KEY (org_id, webhook_work_id) REFERENCES public.webhook_work(org_id, work_id);

ALTER TABLE ONLY public.local_password
    ADD CONSTRAINT local_password_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.operator_session
    ADD CONSTRAINT operator_session_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.organization_membership
    ADD CONSTRAINT organization_membership_user_id_fkey FOREIGN KEY (user_id) REFERENCES public.app_user(user_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.postmortem
    ADD CONSTRAINT postmortem_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_investigation_belongs_to_organization FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.relay_job
    ADD CONSTRAINT relay_job_relay_is_in_the_same_org FOREIGN KEY (org_id, registration_id) REFERENCES public.relay_registration(org_id, registration_id);

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.slack_conversation
    ADD CONSTRAINT slack_conversation_is_in_the_same_org FOREIGN KEY (org_id, conversation_id) REFERENCES public.conversation(org_id, conversation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.slack_reply
    ADD CONSTRAINT slack_reply_investigation_is_in_the_same_org FOREIGN KEY (org_id, investigation_id) REFERENCES public.investigation(org_id, investigation_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_delivery_is_in_the_same_org FOREIGN KEY (org_id, delivery_id) REFERENCES public.integration_delivery(org_id, delivery_id) ON DELETE CASCADE;

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_incident_is_in_the_same_org FOREIGN KEY (org_id, incident_id) REFERENCES public.incident(org_id, incident_id);

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_integration_is_in_the_same_org FOREIGN KEY (org_id, integration_id) REFERENCES public.integration(org_id, integration_id);

ALTER TABLE ONLY public.webhook_work
    ADD CONSTRAINT webhook_work_message_is_in_the_same_org FOREIGN KEY (org_id, conversation_id, message_sequence) REFERENCES public.conversation_message(org_id, conversation_id, sequence);

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

INSERT INTO public.integration_type
    (integration_type_id, key, name, description, logo, category)
VALUES
    (1, 'alertmanager', 'Prometheus Alertmanager', 'Create incidents from firing and resolved Alertmanager alerts delivered through an authenticated webhook.', 'alertmanager', 'alerting'),
    (2, 'kubernetes', 'Kubernetes', 'Give investigations read-only access to Kubernetes workload runtime, namespace events, and bounded container logs through an outbound Relay.', 'kubernetes', 'infrastructure'),
    (3, 'slack', 'Slack', 'Give investigations read-only access to Slack conversations visible to the connected token and reply to direct app mentions in their original thread.', 'slack', 'collaboration'),
    (4, 'github', 'GitHub', 'Give investigations read-only access to selected repositories for commits, pull requests, CI failures, files, and releases.', 'github', 'source-control'),
    (5, 'generic_webhook', 'Generic Webhook', 'Create incidents from canonical firing and resolved Alert Events delivered through an authenticated webhook.', '', 'alerting');
