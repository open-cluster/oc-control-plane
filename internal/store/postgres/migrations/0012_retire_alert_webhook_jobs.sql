DO $$
BEGIN
    IF to_regclass('public.webhook_job') IS NOT NULL AND
       to_regclass('public.investigation') IS NOT NULL THEN
        IF EXISTS (
            SELECT 1 FROM investigation AS investigation
            JOIN webhook_job AS job
              ON job.org_id = investigation.org_id AND job.job_id = investigation.webhook_job_id
            WHERE job.kind = 1 AND
                (investigation.incident_id IS DISTINCT FROM job.incident_id OR
                 investigation.conversation_id IS NOT NULL)
        ) THEN
            RAISE EXCEPTION 'legacy alert Investigation does not match its Incident';
        END IF;

        UPDATE investigation AS investigation
           SET automatic_incident = true
          FROM webhook_job AS job
         WHERE job.org_id = investigation.org_id AND job.job_id = investigation.webhook_job_id
           AND job.kind = 1;

        INSERT INTO investigation
            (investigation_id, org_id, incident_id, subject, window_from, window_until,
             created_by, created_at, automatic_incident)
        SELECT gen_random_uuid(), incident.org_id, incident.incident_id, incident.title,
               incident.first_seen_at - interval '2 hours', incident.last_seen_at,
               'webhook', legacy.created_at, true
          FROM (
              SELECT org_id, incident_id, min(created_at) AS created_at
                FROM webhook_job WHERE kind = 1
               GROUP BY org_id, incident_id
          ) AS legacy
          JOIN incident ON incident.org_id = legacy.org_id AND incident.incident_id = legacy.incident_id
         WHERE NOT EXISTS (
             SELECT 1 FROM investigation
              WHERE investigation.org_id = incident.org_id
                AND investigation.incident_id = incident.incident_id
                AND investigation.automatic_incident
         );

        UPDATE investigation AS investigation
           SET webhook_job_id = NULL
          FROM webhook_job AS job
         WHERE job.org_id = investigation.org_id AND job.job_id = investigation.webhook_job_id
           AND job.kind = 1;

        DELETE FROM webhook_job WHERE kind = 1;
    END IF;
END $$;
