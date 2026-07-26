// Package config loads and validates the control plane's process configuration from the
// environment. Configuration is non-secret with one deliberate exception: a placement's
// connection string carries a password, so placements name a FILE holding the DSN rather
// than the DSN itself. No environment value ever carries a credential, and no error ever
// quotes a DSN file's contents — a failed start must not write a password into a log.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for the optional settings.
const (
	defaultShutdownTimeout = 15 * time.Second
	defaultServiceName     = "oc-control-plane"
)

// Environment variable names, listed once so errors and documentation cannot drift.
const (
	EnvHTTPAddress      = "OC_HTTP_ADDRESS"
	EnvPlacements       = "OC_PLACEMENTS"
	EnvAssignments      = "OC_PLACEMENT_ASSIGNMENTS"
	EnvDefaultPlacement = "OC_DEFAULT_PLACEMENT"
	EnvShutdownTimeout  = "OC_SHUTDOWN_TIMEOUT"
	EnvServiceName      = "OC_SERVICE_NAME"
	EnvOTLPEndpoint     = "OC_OTLP_ENDPOINT"
)

// Config is the validated process configuration.
type Config struct {
	// HTTPAddress is the listen address for health, readiness, and metrics.
	HTTPAddress string

	// Placements maps a placement name to its resolved connection string. The DSN is read
	// from the file the operator named; it is never carried in an environment value.
	Placements map[string]string

	// Assignments maps an organization to its placement name. An organization listed here
	// overrides the default, which is how the Business and Enterprise tiers put a tenant on
	// a dedicated database.
	Assignments map[string]string

	// DefaultPlacement serves organizations with no explicit assignment. It is the shared
	// tier: enumerating five thousand organizations in an environment variable, and
	// restarting every instance to onboard one, is not a deployment.
	//
	// It is OPTIONAL and, when set, is an explicit operator declaration — not an implicit
	// fallback. With no default configured an unassigned organization is a hard error,
	// because silently serving an unrecognised caller from someone else's connection is the
	// failure this design exists to prevent.
	DefaultPlacement string

	// ShutdownTimeout bounds the drain of in-flight requests on SIGTERM.
	ShutdownTimeout time.Duration

	// ServiceName identifies this process in telemetry.
	ServiceName string

	// OTLPEndpoint is the trace collector, host:port. Empty disables trace export, which
	// is the correct default for a process with no collector configured.
	OTLPEndpoint string
}

// Load reads configuration through lookup (os.LookupEnv in production) and validates every
// value, failing on the first problem and naming the offending variable.
func Load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		ShutdownTimeout: defaultShutdownTimeout,
		ServiceName:     defaultServiceName,
	}

	var err error
	if cfg.HTTPAddress, err = requiredListenAddress(lookup, EnvHTTPAddress); err != nil {
		return Config{}, err
	}
	if cfg.Placements, err = placements(lookup); err != nil {
		return Config{}, err
	}
	if cfg.Assignments, err = assignments(lookup, cfg.Placements); err != nil {
		return Config{}, err
	}
	if cfg.DefaultPlacement, err = defaultPlacement(lookup, cfg.Placements); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = optionalDuration(lookup, EnvShutdownTimeout, cfg.ShutdownTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ServiceName, err = optionalName(lookup, EnvServiceName, cfg.ServiceName); err != nil {
		return Config{}, err
	}
	if cfg.OTLPEndpoint, err = optionalHostPort(lookup, EnvOTLPEndpoint); err != nil {
		return Config{}, err
	}

	// A deployment with neither explicit assignments nor a default could resolve no
	// organization at all, which is a misconfiguration rather than a strict posture.
	if len(cfg.Assignments) == 0 && cfg.DefaultPlacement == "" {
		return Config{}, fmt.Errorf(
			"%s or %s is required: with neither, no organization can be resolved",
			EnvAssignments, EnvDefaultPlacement)
	}
	return cfg, nil
}

// placements parses "name=/path/to/dsn" pairs and resolves each DSN from its file. The
// file's contents are trimmed and never appear in an error.
func placements(lookup func(string) (string, bool)) (map[string]string, error) {
	raw, err := required(lookup, EnvPlacements)
	if err != nil {
		return nil, err
	}

	resolved := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		name, path, found := strings.Cut(strings.TrimSpace(entry), "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !found || name == "" || path == "" {
			return nil, fmt.Errorf("%s: each entry must be name=path-to-dsn-file", EnvPlacements)
		}
		if _, duplicate := resolved[name]; duplicate {
			return nil, fmt.Errorf("%s: placement %q is defined more than once", EnvPlacements, name)
		}

		dsn, readErr := readDSN(path)
		if readErr != nil {
			return nil, fmt.Errorf("%s: placement %q: %w", EnvPlacements, name, readErr)
		}
		resolved[name] = dsn
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("%s: no usable placements", EnvPlacements)
	}
	return resolved, nil
}

// readDSN reads a placement's connection string from disk. Every error is classified
// rather than wrapped, because the underlying error can quote file contents — which for
// this file is a database password.
func readDSN(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errors.New("dsn file does not exist")
		}
		if errors.Is(err, os.ErrPermission) {
			return "", errors.New("dsn file is not readable")
		}
		return "", errors.New("dsn file could not be read")
	}
	dsn := strings.TrimSpace(string(raw))
	if dsn == "" {
		return "", errors.New("dsn file is empty")
	}
	return dsn, nil
}

// assignments parses "org=placement" pairs. Every named placement must exist, so a typo
// fails at startup rather than becoming an unresolvable organization at request time.
//
// The variable is optional: a deployment with a default placement puts every organization
// on it and names only the exceptions. A deployment with neither is refused by Load, since
// it could resolve nothing.
func assignments(lookup func(string) (string, bool), known map[string]string) (map[string]string, error) {
	raw, ok := lookup(EnvAssignments)
	if !ok || strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	raw = strings.TrimSpace(raw)

	resolved := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		organization, placement, found := strings.Cut(strings.TrimSpace(entry), "=")
		organization, placement = strings.TrimSpace(organization), strings.TrimSpace(placement)
		if !found || organization == "" || placement == "" {
			return nil, fmt.Errorf("%s: each entry must be organization=placement", EnvAssignments)
		}
		if _, ok := known[placement]; !ok {
			return nil, fmt.Errorf("%s: organization %q names unknown placement %q",
				EnvAssignments, organization, placement)
		}
		resolved[organization] = placement
	}
	return resolved, nil
}

// defaultPlacement resolves the optional shared-tier placement. Naming a placement that
// does not exist is refused here so the mistake is a startup failure rather than an
// unresolvable organization at request time.
func defaultPlacement(lookup func(string) (string, bool), known map[string]string) (string, error) {
	value, ok := lookup(EnvDefaultPlacement)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	name := strings.TrimSpace(value)
	if _, defined := known[name]; !defined {
		return "", fmt.Errorf("%s names unknown placement %q", EnvDefaultPlacement, name)
	}
	return name, nil
}

func required(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return strings.TrimSpace(value), nil
}

func requiredListenAddress(lookup func(string) (string, bool), key string) (string, error) {
	value, err := required(lookup, key)
	if err != nil {
		return "", err
	}
	if err := validateHostPort(value); err != nil {
		return "", fmt.Errorf("%s must be host:port: %w", key, err)
	}
	return value, nil
}

func optionalHostPort(lookup func(string) (string, bool), key string) (string, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return "", nil
	}
	value = strings.TrimSpace(value)
	if err := validateHostPort(value); err != nil {
		return "", fmt.Errorf("%s must be host:port: %w", key, err)
	}
	return value, nil
}

// validateHostPort accepts only a bare listen or dial address: an optional host with no
// scheme and no path, plus a usable port. net.SplitHostPort alone would accept
// "http://host:8080" by splitting on the scheme's colon.
func validateHostPort(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if strings.ContainsAny(host, "/\\") {
		return fmt.Errorf("invalid host %q", host)
	}
	if strings.Contains(port, "/") {
		return fmt.Errorf("invalid port %q", port)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65_535 {
		return fmt.Errorf("invalid port %q", port)
	}
	return nil
}

func optionalDuration(
	lookup func(string) (string, bool), key string, fallback time.Duration,
) (time.Duration, error) {
	value, ok := lookup(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return parsed, nil
}

func optionalName(lookup func(string) (string, bool), key, fallback string) (string, error) {
	value, ok := lookup(key)
	if !ok {
		return fallback, nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s must not be blank", key)
	}
	return trimmed, nil
}
