ALTER TABLE public.investigation_tool_run
    ADD COLUMN tool_run_id uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN execution_id uuid,
    DROP CONSTRAINT investigation_tool_run_pkey,
    ALTER COLUMN investigation_id DROP NOT NULL,
    ALTER COLUMN ordinal DROP NOT NULL,
    ALTER COLUMN outcome DROP NOT NULL,
    ALTER COLUMN finished_at DROP NOT NULL,
    ADD PRIMARY KEY (org_id, tool_run_id),
    ADD UNIQUE (org_id, investigation_id, ordinal),
    ADD FOREIGN KEY (org_id, execution_id)
        REFERENCES public.agent_execution (org_id, execution_id),
    ADD CHECK (
        (execution_id IS NULL AND investigation_id IS NOT NULL AND ordinal IS NOT NULL AND outcome IS NOT NULL)
        OR (execution_id IS NOT NULL AND investigation_id IS NULL AND ordinal IS NULL)
    ),
    ADD CHECK ((outcome IS NULL) = (finished_at IS NULL));

CREATE INDEX investigation_tool_run_execution_idx
    ON public.investigation_tool_run (org_id, execution_id) WHERE execution_id IS NOT NULL;

ALTER TABLE public.relay_job
    ADD COLUMN tool_run_id uuid,
    ADD FOREIGN KEY (org_id, tool_run_id)
        REFERENCES public.investigation_tool_run (org_id, tool_run_id),
    ADD CHECK (tool_run_id IS NULL OR investigation_id IS NULL);

CREATE UNIQUE INDEX relay_job_tool_run_idx
    ON public.relay_job (org_id, tool_run_id) WHERE tool_run_id IS NOT NULL;

-- Keep ordinal anchors for existing readers and leave pruned observations unresolved.
CREATE FUNCTION pg_temp.add_tool_run_ids(entries jsonb, organization uuid, investigation uuid)
RETURNS jsonb LANGUAGE sql STABLE AS $$
    SELECT coalesce(jsonb_agg(
        CASE WHEN citations.value IS NULL THEN entry.value
             ELSE jsonb_set(entry.value, '{evidenceRefs}', citations.value) END
        ORDER BY entry.position), entries)
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(entries) = 'array' THEN entries END)
        WITH ORDINALITY AS entry(value, position)
    CROSS JOIN LATERAL (
        SELECT jsonb_agg(
            CASE WHEN run.tool_run_id IS NOT NULL AND NOT (ref.value ? 'toolRunId')
                 THEN ref.value || jsonb_build_object('toolRunId', run.tool_run_id)
                 ELSE ref.value END
            ORDER BY ref.from_ordinals, ref.position) AS value
        FROM (
            SELECT value, position, false AS from_ordinals
            FROM jsonb_path_query(entry.value, 'strict $.evidenceRefs[*]', '{}', true)
                WITH ORDINALITY AS existing(value, position)
            UNION ALL
            SELECT jsonb_build_object('investigationId', coalesce(investigation::text, entry.value ->> 'investigationId'),
                                      'toolRunOrdinal', value), position, true
            FROM jsonb_path_query(entry.value, 'strict $.runRefs[*]', '{}', true)
                WITH ORDINALITY AS ordinal(value, position)
        ) ref
        LEFT JOIN public.investigation_tool_run run
          ON run.org_id = organization
         AND run.investigation_id = CASE WHEN pg_input_is_valid(ref.value ->> 'investigationId', 'uuid')
                                        THEN (ref.value ->> 'investigationId')::uuid END
         AND run.ordinal = CASE WHEN pg_input_is_valid(ref.value ->> 'toolRunOrdinal', 'integer')
                                THEN (ref.value ->> 'toolRunOrdinal')::integer END
        WHERE NOT ref.from_ordinals
           OR (run.tool_run_id IS NOT NULL
               AND NOT coalesce(entry.value -> 'evidenceRefs' @> jsonb_build_array(ref.value), false))
    ) citations;
$$;

UPDATE public.investigation
SET conclusion = jsonb_set(conclusion, '{findings}', pg_temp.add_tool_run_ids(conclusion -> 'findings', org_id, investigation_id))
WHERE jsonb_typeof(conclusion -> 'findings') = 'array';

UPDATE public.postmortem p SET document = document || (
    SELECT jsonb_object_agg(section.key, pg_temp.add_tool_run_ids(p.document -> section.key, p.org_id, NULL))
    FROM (VALUES ('timeline'), ('rootCauses'), ('contributingFactors')) AS section(key)
    WHERE p.document ? section.key
)
WHERE jsonb_typeof(document) = 'object'
  AND document ?| ARRAY['timeline', 'rootCauses', 'contributingFactors'];

DROP FUNCTION pg_temp.add_tool_run_ids(jsonb, uuid, uuid);

ALTER TABLE public.postmortem ADD COLUMN source_restricted boolean NOT NULL DEFAULT false;

-- Older snapshots flattened testimony and conclusions into prose. Preserve the
-- snapshot and its review state, but withhold it when Slack was a possible input.
UPDATE public.postmortem p SET source_restricted = true
WHERE EXISTS (
    SELECT 1 FROM public.conversation c
    LEFT JOIN public.investigation i
        ON i.org_id = c.org_id AND i.conversation_id = c.conversation_id
    WHERE c.org_id = p.org_id AND c.source = 'slack' AND c.created_at <= p.updated_at
      AND ((c.incident_id = p.incident_id AND EXISTS (
               SELECT 1 FROM public.conversation_message m
               WHERE m.org_id = c.org_id AND m.conversation_id = c.conversation_id
                 AND m.created_at <= p.updated_at AND btrim(m.text) <> ''))
           OR (i.incident_id = p.incident_id AND i.created_at <= p.updated_at)
           OR i.investigation_id::text IN (
               SELECT ref.value #>> '{}' FROM jsonb_path_query(p.document, '$.**.investigationId') ref(value)))
);
