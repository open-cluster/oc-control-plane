package config

import "fmt"

func LoadRecoveryDatabase(lookup func(string) (string, bool)) (string, error) {
	effective, err := effectiveLookup(lookup)
	if err != nil {
		return "", err
	}
	lookup = effective
	dsn, err := readSecretText(lookup, EnvDatabaseDSNFile)
	if err != nil {
		return "", err
	}
	if dsn == "" {
		return "", fmt.Errorf("%s or %s is required", EnvDatabaseDSN, EnvDatabaseDSNFile)
	}
	return dsn, nil
}
