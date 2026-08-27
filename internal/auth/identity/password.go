package identity

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMemory      = 64 * 1024
	passwordIterations  = 3
	passwordParallelism = 4
	passwordSaltBytes   = 16
	passwordKeyBytes    = 32
	minPasswordBytes    = 12
	maxPasswordBytes    = 1024
)

var errPasswordFormat = errors.New("local password verifier has an unusable format")

func hashPassword(password string) (string, error) {
	if len(password) < minPasswordBytes || len(password) > maxPasswordBytes {
		return "", fmt.Errorf("password must be between %d and %d bytes",
			minPasswordBytes, maxPasswordBytes)
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("minting a password salt: %w", err)
	}
	return encodePassword(password, salt, passwordMemory, passwordIterations,
		passwordParallelism), nil
}

func encodePassword(password string, salt []byte, memory, iterations uint32, parallelism uint8) string {
	key := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, passwordKeyBytes)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func verifyPassword(encoded, password string) (bool, bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, false, errPasswordFormat
	}
	version, err := parsePasswordParameter(parts[2], "v")
	if err != nil || version != uint64(argon2.Version) {
		return false, false, errPasswordFormat
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return false, false, errPasswordFormat
	}
	memory, err := parsePasswordParameter(parameters[0], "m")
	if err != nil {
		return false, false, errPasswordFormat
	}
	iterations, err := parsePasswordParameter(parameters[1], "t")
	if err != nil {
		return false, false, errPasswordFormat
	}
	parallel, err := parsePasswordParameter(parameters[2], "p")
	if err != nil || parallel > 255 || memory == 0 || iterations == 0 {
		return false, false, errPasswordFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false, false, errPasswordFormat
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) != passwordKeyBytes {
		return false, false, errPasswordFormat
	}
	got := argon2.IDKey([]byte(password), salt, uint32(iterations), uint32(memory),
		uint8(parallel), uint32(len(want)))
	valid := subtle.ConstantTimeCompare(got, want) == 1
	needsRehash := memory != passwordMemory || iterations != passwordIterations ||
		parallel != passwordParallelism
	return valid, needsRehash, nil
}

func parsePasswordParameter(raw, name string) (uint64, error) {
	prefix := name + "="
	if !strings.HasPrefix(raw, prefix) {
		return 0, errPasswordFormat
	}
	value, err := strconv.ParseUint(strings.TrimPrefix(raw, prefix), 10, 32)
	if err != nil {
		return 0, errPasswordFormat
	}
	return value, nil
}

var dummyPasswordHash = encodePassword("password that is never accepted",
	[]byte("fixed timing salt"), passwordMemory, passwordIterations, passwordParallelism)
