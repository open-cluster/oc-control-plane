package audit

import (
	"sort"
	"strings"
)

// credentialWords are the substrings that make a key's value unfit for the record.
//
// The list is deliberately broad. A false positive costs an auditor one field of context; a
// false negative writes a live credential into a table the database will not let anyone delete
// from, on every database, forever.
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

// Detail is the structured context one event carries — the previous and new value of a changed
// setting, the reason a request was refused, the count something acted on.
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
	if len(keys) > MaxDetailEntries {
		keys = keys[:MaxDetailEntries]
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

func safeDetailForAction(action Action, detail Detail) Detail {
	if action == ActionPolicyChanged {
		return safePolicyChangeDetail(detail)
	}
	return detail.Safe()
}

func safePolicyChangeDetail(detail Detail) Detail {
	before := safeRetentionMetadata(detail["before"])
	after := safeRetentionMetadata(detail["after"])
	if before == nil && after == nil {
		return nil
	}
	safe := Detail{}
	if before != nil {
		safe["before"] = before
	}
	if after != nil {
		safe["after"] = after
	}
	return safe
}

func safeRetentionMetadata(value any) Detail {
	detail, ok := value.(map[string]any)
	if !ok {
		if typed, isDetail := value.(Detail); isDetail {
			detail = typed
			ok = true
		}
	}
	if !ok {
		return nil
	}
	value, ok = detail["auditRetentionDays"]
	if !ok {
		return nil
	}
	return Detail{"auditRetentionDays": boundedValue(value)}
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
		return truncate(typed, MaxDetailValueLength)
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
