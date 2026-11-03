package mcpserver

import "slices"

// pushInputSchema projects the public commit contract into the local workflow.
// RawMessage is wire JSON, not []byte: Go reflection would advertise it as a
// byte array. Keep all domain fields and references from the embedded OpenAPI,
// adapting only the local project selection and computable file hashes/sizes.
func (a *adapter) pushInputSchema() (map[string]any, error) {
	input, err := a.localSchema(map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"directory", "state", "input"},
		"properties": map[string]any{
			"directory": map[string]any{"type": "string"},
			"state":     map[string]any{"type": "string"},
			"input":     map[string]any{"$ref": "#/components/schemas/CommitRequest"},
		},
	})
	if err != nil {
		return nil, err
	}
	defs := input["$defs"].(map[string]any)
	commit := defs["CommitRequest"].(map[string]any)
	commit["properties"].(map[string]any)["project_id"] = map[string]any{"$ref": "#/$defs/Ulid"}
	commit["required"] = append(commit["required"].([]any), "project_id")

	// A local working copy need not declare hashes or sizes. Empty sha256 means
	// compute it; a nonempty digest still has to be a valid public SHA-256 value.
	file := defs["VersionManifestFile"].(map[string]any)
	omitRequired(file, "sha256", "size")
	props := file["properties"].(map[string]any)
	props["sha256"] = map[string]any{"anyOf": []any{props["sha256"], map[string]any{"const": ""}}}

	return input, nil
}

// Remove only local-input exceptions, retaining any future domain requirements.
func omitRequired(schema map[string]any, names ...string) {
	schema["required"] = slices.DeleteFunc(schema["required"].([]any), func(value any) bool {
		name, ok := value.(string)
		return ok && slices.Contains(names, name)
	})
}
