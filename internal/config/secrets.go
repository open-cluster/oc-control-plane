package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// SecretSource is the deployment boundary for secret material. The reference identifies
// where the source reads; it is never itself returned in an error or log.
type SecretSource interface {
	Read(setting, reference string) ([]byte, error)
}

// MountedSecretSource reads the files projected by an orchestrator or external secret system.
// OpenCluster therefore needs no Vault, KMS, or cloud-secret SDK to consume those systems.
type MountedSecretSource struct{}

func (MountedSecretSource) Read(setting, reference string) ([]byte, error) {
	value, err := os.ReadFile(reference)
	if err != nil {
		classified := errors.New("file could not be read")
		if errors.Is(err, os.ErrNotExist) {
			classified = errors.New("file does not exist")
		} else if errors.Is(err, os.ErrPermission) {
			classified = errors.New("file is not readable")
		}
		if setting == "" {
			return nil, classified
		}
		return nil, fmt.Errorf("%s: secret %w", setting, classified)
	}
	return value, nil
}

// EnvironmentSecretSource is an explicit escape hatch for local development harnesses. It is
// deliberately not composed into production configuration, where environment values name
// files only. Every successful read warns without logging the reference or value.
type EnvironmentSecretSource struct {
	Development bool
	Lookup      func(string) (string, bool)
	Logger      *slog.Logger
}

func (s EnvironmentSecretSource) Read(setting, reference string) ([]byte, error) {
	if !s.Development {
		return nil, fmt.Errorf("%s: environment secret values are development-only", setting)
	}
	if s.Lookup == nil {
		return nil, fmt.Errorf("%s: environment secret source has no lookup", setting)
	}
	value, present := s.Lookup(reference)
	if !present || strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("%s: development secret is not set", setting)
	}
	if s.Logger == nil {
		return nil, fmt.Errorf("%s: development secret source requires a warning logger", setting)
	}
	s.Logger.Warn("environment secret values are development-only", slog.String("setting", setting))
	return []byte(value), nil
}
