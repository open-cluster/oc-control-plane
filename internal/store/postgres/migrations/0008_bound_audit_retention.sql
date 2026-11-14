LOCK TABLE public.organization IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.organization WHERE audit_retention_days > 365) THEN
        RAISE EXCEPTION 'audit retention exceeds 365 days for at least one Organization'
            USING HINT = 'Use the previous version to set affected policies to 0 to preserve records without automatic pruning, or choose 1 to 365 days. Then retry the upgrade.';
    END IF;
END $$;

ALTER TABLE public.organization
    DROP CONSTRAINT organization_audit_retention_days_check,
    ADD CONSTRAINT organization_audit_retention_days_check
        CHECK (audit_retention_days BETWEEN 0 AND 365);
