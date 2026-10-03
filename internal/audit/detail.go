package audit

import (
	"sort"
	"strings"
)

// False positives omit one audit detail; false negatives persist a live credential permanently.
var credentialWords = []string{
	"secret",
	"token",
	"password",
	"passwd",
	"credential",
	"apikey",
	"api_key",
	"privatekey",
	"private_key",
	"authorization",
	"cookie",
	"digest",
	"signature",
	"assertion",
}

type Detail map[string]any

func (d Detail) Safe() Detail {
	if len(d) == 0 {
		return nil
	}

	keys := make([]string, 0, len(d))
	for key := range d {
		if !namesACredential(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > maxAuditDetailEntries {
		keys = keys[:maxAuditDetailEntries]
	}

	safe := make(Detail, len(keys))
	for _, key := range keys {
		safe[key] = boundedValue(d[key])
	}
	if len(safe) == 0 {
		return nil
	}
	return safe
}

func NamesACredential(key string) bool { return namesACredential(key) }

func namesACredential(key string) bool {
	folded := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
	for _, word := range credentialWords {
		if strings.Contains(folded, strings.ReplaceAll(word, "_", "")) {
			return true
		}
	}
	return false
}

func boundedValue(value any) any {
	switch typed := value.(type) {
	case string:
		return truncate(typed, maxAuditTextLength)
	case Detail:
		return typed.Safe()
	case map[string]any:
		return Detail(typed).Safe()
	case map[string]string:
		nested := make(Detail, len(typed))
		for key, nestedValue := range typed {
			nested[key] = nestedValue
		}
		return nested.Safe()
	default:
		return value
	}
}
