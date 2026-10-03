package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type relay struct {
	program *program
	output  *syncBuffer
	starts  int

	name           string
	credentialPath string
	tokenPath      string
	environment    map[string]string
}

type relayInstallation struct {
	Name                string
	WorkDir             string
	Token               string
	ControlPlaneAddress string
	SPKIPin             string
	Organization        string
	KubeconfigPath      string
	Extra               map[string]string
}

func newRelay(installation relayInstallation) (*relay, error) {
	credentialPath := filepath.Join(installation.WorkDir, installation.Name+"-credential.json")
	tokenPath := filepath.Join(installation.WorkDir, installation.Name+"-token")

	if err := os.WriteFile(tokenPath, []byte(installation.Token), 0o600); err != nil {
		return nil, fmt.Errorf("writing the bootstrap token: %w", err)
	}

	inventoryPath := filepath.Join(installation.WorkDir, installation.Name+"-inventory.yaml")
	if err := os.WriteFile(inventoryPath,
		[]byte("inventory:\n  version: 1\n  minimum_interval: 1s\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the inventory configuration: %w", err)
	}

	installed := &relay{
		output:         &syncBuffer{},
		name:           installation.Name,
		credentialPath: credentialPath,
		tokenPath:      tokenPath,
		environment: map[string]string{
			"RELAY_CONTROL_PLANE_ADDRESS": installation.ControlPlaneAddress,
			"RELAY_ORG_ID":                installation.Organization,
			"RELAY_CREDENTIAL_FILE":       credentialPath,
			"RELAY_BOOTSTRAP_TOKEN_FILE":  tokenPath,
			"RELAY_KUBECONFIG":            installation.KubeconfigPath,
			"RELAY_INITIAL_SPKI_PINS":     installation.SPKIPin,
			"RELAY_HEARTBEAT_INTERVAL":    "2s",
			"RELAY_RESEND_INTERVAL":       "2s",
			"RELAY_INVENTORY_CONFIG_FILE": inventoryPath,
		},
	}
	for key, value := range installation.Extra {
		installed.environment[key] = value
	}
	return installed, nil
}

func (r *relay) start() error {
	binary, err := relayBinary()
	if err != nil {
		return err
	}
	r.starts++
	r.output.mark(fmt.Sprintf("relay %s, start %d", r.name, r.starts))

	running, err := startProgram("relay "+r.name, binary, r.environment, r.output)
	if err != nil {
		return err
	}
	r.program = running
	return nil
}

func (r *relay) stop() {
	if r == nil || r.program == nil {
		return
	}
	r.program.kill()
}

func (r *relay) logs() string {
	if r == nil {
		return ""
	}
	return r.output.String()
}

func (r *relay) enrolled() (bool, error) {
	info, err := os.Stat(r.credentialPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the relay's credential file: %w", err)
	}
	return info.Size() > 0, nil
}
