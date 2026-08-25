package config

import (
	"fmt"
	"strconv"
	"strings"
)

// hosted keeps commercial providers outside the self-hosted execution path unless
// deployment ownership explicitly enables them and supplies file-backed credentials.
func hosted(lookup func(string) (string, bool), cfg *Config) error {
	rawMode, _ := lookup(EnvHostedMode)
	if strings.TrimSpace(rawMode) != "" {
		enabled, err := strconv.ParseBool(strings.TrimSpace(rawMode))
		if err != nil {
			return fmt.Errorf("%s must be true or false", EnvHostedMode)
		}
		cfg.HostedMode = enabled
	}
	keyPath, _ := lookup(EnvWorkOSAPIKeyFile)
	mappings, _ := lookup(EnvWorkOSAuditOrganizations)
	endpoint, _ := lookup(EnvWorkOSAPIURL)
	configured := strings.TrimSpace(keyPath) != "" || strings.TrimSpace(mappings) != "" ||
		strings.TrimSpace(endpoint) != ""
	if !cfg.HostedMode {
		if configured {
			return fmt.Errorf("WorkOS settings require %s=true", EnvHostedMode)
		}
		return nil
	}
	if cfg.AuthenticationMode != "local+oidc" || cfg.OIDCIssuer == "" {
		return fmt.Errorf("%s requires WorkOS authentication through configured OIDC", EnvHostedMode)
	}
	if strings.TrimSpace(keyPath) == "" {
		return fmt.Errorf("%s is required in hosted mode", EnvWorkOSAPIKeyFile)
	}
	secret, err := (MountedSecretSource{}).Read(EnvWorkOSAPIKeyFile,
		strings.TrimSpace(keyPath))
	if err != nil {
		return err
	}
	cfg.WorkOSAPIKey = strings.TrimSpace(string(secret))
	if cfg.WorkOSAPIKey == "" {
		return fmt.Errorf("%s: secret file is empty", EnvWorkOSAPIKeyFile)
	}
	if cfg.WorkOSAPIURL, err = optionalVendorURL(lookup, EnvWorkOSAPIURL); err != nil {
		return err
	}
	if strings.TrimSpace(mappings) == "" {
		return fmt.Errorf("%s is required in hosted mode", EnvWorkOSAuditOrganizations)
	}
	cfg.WorkOSAuditOrganizations = map[string]string{}
	for _, entry := range strings.Split(mappings, ",") {
		local, remote, valid := strings.Cut(strings.TrimSpace(entry), "=")
		local, remote = strings.TrimSpace(local), strings.TrimSpace(remote)
		if !valid || local == "" || remote == "" || cfg.WorkOSAuditOrganizations[local] != "" {
			return fmt.Errorf("%s must contain unique organization=workos_organization pairs",
				EnvWorkOSAuditOrganizations)
		}
		cfg.WorkOSAuditOrganizations[local] = remote
	}
	return nil
}
