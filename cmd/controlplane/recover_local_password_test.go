package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestRecoveryRejectsPasswordArgumentsWithoutEcho(t *testing.T) {
	const secret = "do-not-echo-this-password"
	err := recoverLocalPassword(context.Background(), []string{"--user", "5ce58d96-3e95-47c0-ab52-29d688235df0", "--password", secret}, nil, io.Discard, func(string) (string, bool) { return "", false })
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe argument refusal: %v", err)
	}
}
