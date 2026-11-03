// Package managementrecord owns the shared private record envelope and payload
// guard used by the core, official clients and web schema projection. It owns
// no storage, authorization decision, approval, job state or command receipt.
package managementrecord

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/schema"
	"github.com/oujinhaoai/lantai/schemas"
)

const Contract = "lantai.management-record/v1"

type Envelope struct {
	Schema     string          `json:"schema"`
	Kind       string          `json:"kind"`
	InstanceID ids.ID          `json:"instance_id"`
	ProjectID  ids.ID          `json:"project_id"`
	Payload    json.RawMessage `json:"payload"`
}

type Rules struct {
	MaxRecordBytes  int      `json:"max_record_bytes"`
	MaxPayloadDepth int      `json:"max_payload_depth"`
	MaxTitleUnits   int      `json:"max_title_utf16_units"`
	ForbiddenFields []string `json:"forbidden_fields"`
}

func Definition() (Rules, []string, error) {
	raw, err := schemas.FS.ReadFile("catalog/v1/management-record.schema.json")
	if err != nil {
		return Rules{}, nil, err
	}
	var definition struct {
		Rules      Rules `json:"x-management-rules"`
		Properties struct {
			Kind struct {
				Enum []string `json:"enum"`
			} `json:"kind"`
		} `json:"properties"`
	}
	if err = json.Unmarshal(raw, &definition); err != nil {
		return Rules{}, nil, err
	}
	if definition.Rules.MaxRecordBytes < 1 || definition.Rules.MaxPayloadDepth < 1 || len(definition.Properties.Kind.Enum) == 0 {
		return Rules{}, nil, errors.New("invalid shared management-record definition")
	}
	return definition.Rules, definition.Properties.Kind.Enum, nil
}

func ValidatePayload(raw []byte) error {
	rules, _, err := Definition()
	if err != nil {
		return err
	}
	if len(raw) > rules.MaxRecordBytes {
		return errors.New("management payload exceeds record budget")
	}
	value, err := canonjson.Decode(raw)
	if err != nil {
		return err
	}
	var visit func(any, int) error
	visit = func(value any, depth int) error {
		if depth > rules.MaxPayloadDepth {
			return errors.New("management payload exceeds depth budget")
		}
		switch item := value.(type) {
		case []any:
			for _, v := range item {
				if err := visit(v, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, v := range item {
				if slices.Contains(rules.ForbiddenFields, strings.ToLower(key)) {
					return errors.New("management payload contains a credential or temporary authorization field")
				}
				if err := visit(v, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(value, 0)
}

func Decode(raw []byte) (Envelope, error) {
	var out Envelope
	rules, _, err := Definition()
	if err != nil {
		return out, err
	}
	if len(raw) > rules.MaxRecordBytes {
		return out, errors.New("management record exceeds byte budget")
	}
	tree, err := canonjson.Decode(raw)
	if err != nil {
		return out, err
	}
	registry, err := schema.Default()
	if err != nil {
		return out, err
	}
	if err = registry.Validate(Contract, tree); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if err = ValidatePayload(out.Payload); err != nil {
		return Envelope{}, err
	}
	return out, nil
}

// Render uses the same canonical JSON plus newline convention as the web
// client, making identical inputs reconstructable without a clock or nonce.
func Render(in Envelope) ([]byte, error) {
	raw, err := canonjson.CanonicalizeValue(in)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if _, err = Decode(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
