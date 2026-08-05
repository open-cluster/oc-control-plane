package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// relay is the Relay running as a real process.
//
// Its credential file outlives any single process, which is the point: the first start
// enrols and writes a durable identity, and every start after it loads that identity and
// never touches the bootstrap token again. Giving each start a fresh file would test
// enrolment repeatedly and never test the thing enrolment exists to produce.
type relay struct {
	program *program
	// output spans restarts for the same reason the control plane's does: the interesting
	// thing a Relay says is often said by the process before the one that failed.
	output *syncBuffer
	starts int

	name           string
	credentialPath string
	tokenPath      string
	environment    map[string]string
}

// relayInstallation is what an operator would have to supply to install a Relay. It travels
// as one value because the fields are meaningless apart, and because six adjacent strings in
// a parameter list are two that can be transposed without the compiler noticing.
type relayInstallation struct {
	// Name distinguishes one Relay's files and log from another's within a run.
	Name    string
	WorkDir string
	// Token is the single-use bootstrap credential. A Relay that already has a durable
	// identity ignores it.
	Token               string
	ControlPlaneAddress string
	SPKIPin             string
	Organization        string
	KubeconfigPath      string
	// Extra is per-installation configuration a caller needs and the protocol proof does not.
	// The scenario harness uses it for the one scenario whose subject is the operator's attested
	// event-retention horizon; it is applied last, so a caller can also correct a default above.
	Extra map[string]string
}

// newRelay writes the Relay's files and prepares its environment without starting it.
//
// The bootstrap token goes to a file because the Relay will not read a secret from anywhere
// else — no environment value may carry one — so writing it here is not a harness
// convenience but the only supported way to hand one over.
func newRelay(installation relayInstallation) (*relay, error) {
	credentialPath := filepath.Join(installation.WorkDir, installation.Name+"-credential.json")
	tokenPath := filepath.Join(installation.WorkDir, installation.Name+"-token")

	if err := os.WriteFile(tokenPath, []byte(installation.Token), 0o600); err != nil {
		return nil, fmt.Errorf("writing the bootstrap token: %w", err)
	}

	// The operator's inventory constraints, floored low so a change made by a test is
	// detected within a test's patience. The floor is the customer's to set, and this
	// harness is the customer here.
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
			// Short enough that a reconnect happens inside a test's patience, long enough that
			// the control plane is not being load-tested by its own proof.
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

// enrolled reports whether a durable credential was written, which is the Relay's own record
// that enrolment succeeded. It is read as a file rather than asked for, because a Relay that
// enrolled and then failed to persist would look identical from the control plane's side.
//
// A stat that fails for any reason other than absence is an error rather than a "no". The
// caller uses this to assert that enrolment did NOT happen, and answering "no" to a question
// that could not be read is how that assertion would pass without having checked anything.
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
