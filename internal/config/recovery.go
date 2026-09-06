package config

import (
	"fmt"
	"strings"
)

// LoadRecoveryDatabase reads deployment database configuration without requiring server dependencies.
func LoadRecoveryDatabase(lookup func(string) (string, bool)) (string, error) {
	path, _ := lookup(EnvConfigFile)
	values, err := loadFile(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	dsn, err := databaseDSN(func(key string) (string, bool) {
		if value, ok := lookup(key); ok {
			return value, true
		}
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", fmt.Errorf("%s is required", EnvDatabaseDSNFile)
	}
	return dsn, nil
}
