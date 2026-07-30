// Package api embeds the OpenAPI contract.
//
// The YAML file is the authored source of truth; the JSON form is derived from
// it once at first use, because most tooling asks for JSON while humans and
// diffs are better served by YAML. Deriving rather than committing both means
// the two can never disagree.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed openapi.yaml
var specYAML []byte

// SpecYAML returns the OpenAPI document as authored.
func SpecYAML() []byte { return specYAML }

var (
	jsonOnce sync.Once
	specJSON []byte
	jsonErr  error
)

// SpecJSON returns the OpenAPI document converted to JSON.
//
// The conversion is mechanical: yaml.v3 decodes mappings into map[string]any,
// which encoding/json accepts directly. An error here means the embedded YAML
// is malformed, which the contract tests catch long before a binary ships —
// but the error is still returned rather than panicking, because serving docs
// is never worth crashing the redirect path.
func SpecJSON() ([]byte, error) {
	jsonOnce.Do(func() {
		var doc map[string]any
		if err := yaml.Unmarshal(specYAML, &doc); err != nil {
			jsonErr = fmt.Errorf("api: embedded openapi.yaml does not parse: %w", err)
			return
		}
		specJSON, jsonErr = json.Marshal(doc)
	})
	return specJSON, jsonErr
}
