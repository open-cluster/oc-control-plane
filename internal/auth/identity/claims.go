package identity

import (
	"encoding/json"
	"errors"
	"strings"
)

var ErrTokenRefused = errors.New("the identity token was refused")

type claims struct {
	Issuer        string  `json:"iss"`
	Subject       string  `json:"sub"`
	Nonce         string  `json:"nonce"`
	Email         string  `json:"email"`
	EmailVerified anyBool `json:"email_verified"`
	Name          string  `json:"name"`
	PreferredName string  `json:"preferred_username"`
}

type anyBool bool

func (b *anyBool) UnmarshalJSON(data []byte) error {
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		*b = anyBool(value)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*b = anyBool(strings.EqualFold(text, "true"))
	return nil
}

func (c claims) displayName() string {
	for _, candidate := range []string{c.Name, c.PreferredName} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	if local, _, found := strings.Cut(c.Email, "@"); found && local != "" {
		return local
	}
	return c.Subject
}
