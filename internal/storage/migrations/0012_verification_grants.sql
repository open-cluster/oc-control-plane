-- What the verified credential was granted, in the provider's own vocabulary (Slack:
-- OAuth scopes, plus whether the token is a user token). Tool availability derives from
-- this recorded reality: a tool whose path cannot work with the verified credential is
-- absent from an investigation's set instead of failing at call time. NULL means no
-- grants were recorded — an unverified integration, or a verification that could not
-- read them — and gated tools stay absent.
ALTER TABLE integration
    ADD COLUMN verify_grants JSONB;

COMMENT ON COLUMN integration.verify_grants IS
    'Facts the probe verified about the credential (scopes, token type); tool availability derives from them.';
