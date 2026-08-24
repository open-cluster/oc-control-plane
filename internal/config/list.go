package config

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// structuredListPrefix cannot occur in an operating-system environment value because
// those values cannot contain NUL. It distinguishes the lossless YAML bridge from the
// historical environment grammar, whose bare quotes must remain ordinary characters.
const structuredListPrefix = "\x00yaml-csv:"

// encodeList preserves structured values while using the legacy environment grammar as
// the compatibility bridge. Plain entries remain unchanged; CSV quoting is introduced
// only when an entry contains a delimiter or quote.
func encodeList(entries []string) string {
	if len(entries) == 0 {
		return ""
	}
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	_ = writer.Write(entries)
	writer.Flush()
	return structuredListPrefix + strings.TrimSuffix(encoded.String(), "\n")
}

// decodeList accepts the historical comma-separated form and its CSV-compatible quoted
// extension, so structured YAML values are not made lossy by the compatibility bridge.
func decodeList(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, structuredListPrefix) {
		return strings.Split(raw, ","), nil
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(raw, structuredListPrefix)))
	reader.FieldsPerRecord = -1
	entries, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if _, err := reader.Read(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("must contain one CSV record")
	}
	return entries, nil
}
