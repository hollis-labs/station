package platform

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

var clientIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// LoadClientKeys reads the separate credential registry, never raw tokens.
func LoadClientKeys(path string) ([]KeyConfig, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("platform: auth.keys_file must be absolute")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("platform: read client keys: %w", err)
	}
	var keys []KeyConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&keys); err != nil {
		return nil, fmt.Errorf("platform: decode client keys: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("platform: client keys must contain one YAML document")
	}
	return keys, ValidateClientKeys(keys)
}

// ValidateClientKeys rejects ambiguous identity and credential assignments.
func ValidateClientKeys(keys []KeyConfig) error {
	ids, hashes := map[string]bool{}, map[string]bool{}
	for _, k := range keys {
		if !clientIDPattern.MatchString(k.ID) || !sha256Hex.MatchString(k.SHA256) {
			return fmt.Errorf("platform: invalid client id or SHA-256 digest")
		}
		if ids[k.ID] || hashes[k.SHA256] {
			return fmt.Errorf("platform: duplicate client identity or credential")
		}
		if len(k.Scopes) == 0 {
			return fmt.Errorf("platform: client %q needs explicit scopes", k.ID)
		}
		for _, s := range k.Scopes {
			if s == "" {
				return fmt.Errorf("platform: client %q has empty scope", k.ID)
			}
		}
		ids[k.ID], hashes[k.SHA256] = true, true
	}
	return nil
}
