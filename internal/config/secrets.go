package config

import (
	"fmt"
	"os"
	"strings"
)

func readSecret(lookup func(string) (string, bool), fileSetting string) ([]byte, error) {
	setting := strings.TrimSuffix(fileSetting, "_FILE")
	direct, _ := lookup(setting)
	path, _ := lookup(fileSetting)
	path = strings.TrimSpace(path)
	if direct != "" && path != "" {
		return nil, fmt.Errorf("%s and %s cannot both be set", setting, fileSetting)
	}
	if direct == "" && path == "" {
		return nil, nil
	}
	value := []byte(direct)
	if path != "" {
		var err error
		value, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: secret file cannot be read", fileSetting)
		}
	}
	if strings.TrimSpace(string(value)) == "" {
		return nil, fmt.Errorf("%s: secret is empty", setting)
	}
	return value, nil
}

func readSecretText(lookup func(string) (string, bool), fileSetting string) (string, error) {
	raw, err := readSecret(lookup, fileSetting)
	return strings.TrimSpace(string(raw)), err
}
