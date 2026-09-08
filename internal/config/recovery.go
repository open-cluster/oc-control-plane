package config

import "fmt"

// LoadRecoveryDatabase reads deployment database configuration without requiring server dependencies.
func LoadRecoveryDatabase(lookup func(string) (string, bool)) (string, error) {
	dsn, err := databaseDSN(lookup)
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", fmt.Errorf("%s or %s is required", EnvDatabaseDSN, EnvDatabaseDSNFile)
	}
	return dsn, nil
}
