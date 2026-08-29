ALTER TABLE organization
    ADD COLUMN display_name text;

UPDATE organization
   SET display_name = org_id;

ALTER TABLE organization
    ALTER COLUMN display_name SET NOT NULL,
    ADD CONSTRAINT organization_display_name_check
        CHECK (length(display_name) BETWEEN 1 AND 256);
