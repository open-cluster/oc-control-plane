package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationDocumentationNamesEverySupportedEnvironmentKey(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "self-hosted", "configuration.mdx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range SupportedEnvironmentKeys {
		if !containsWord(content, key) {
			t.Errorf("configuration docs omit %s", key)
		}
	}
}

func containsWord(content []byte, word string) bool {
	for i := 0; i+len(word) <= len(content); i++ {
		if string(content[i:i+len(word)]) == word {
			return true
		}
	}
	return false
}
