package config

import (
	"errors"
	"flag"
	"io"
	"strings"
)

func LoadProcess(args []string, lookup func(string) (string, bool)) (Config, error) {
	if path, _ := lookup("OC_CONFIG_FILE"); strings.TrimSpace(path) != "" {
		return Config{}, errors.New("OC_CONFIG_FILE is retired; use environment configuration")
	}
	flags := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	serverAddress := flags.String("server-address", "", "local server listen address")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return Config{}, errors.New("startup accepts only --server-address; use environment configuration")
	}
	return Load(func(key string) (string, bool) {
		if key == EnvHTTPAddress && strings.TrimSpace(*serverAddress) != "" {
			return *serverAddress, true
		}
		return lookup(key)
	})
}
