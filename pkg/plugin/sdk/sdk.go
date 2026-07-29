// Package sdk contains the stable, dependency-light contract implemented by
// Corral extension executables. External authors may emit the same JSON
// without importing this package.
package sdk

import (
	"encoding/json"
	"fmt"
	"os"
)

const API = "corral.plugin/v1"

type Metadata struct {
	API               string   `json:"api"`
	Name              string   `json:"name"`
	Version           string   `json:"version"`
	Description       string   `json:"description,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
	Permissions       []string `json:"permissions,omitempty"`
	SupportedBackends []string `json:"supportedBackends,omitempty"`
}

// HandleMetadata implements the reserved --corral-plugin-metadata handshake.
// Call it at the beginning of main and return when it reports true.
func HandleMetadata(meta Metadata) bool {
	if len(os.Args) != 2 || os.Args[1] != "--corral-plugin-metadata" {
		return false
	}
	if meta.API == "" {
		meta.API = API
	}
	if err := json.NewEncoder(os.Stdout).Encode(meta); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return true
}
