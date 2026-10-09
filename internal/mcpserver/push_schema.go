package mcpserver

import "slices"

// pushInputSchema projects the public commit contract into the local workflow.
// RawMessage is wire JSON, not []byte: Go reflection would advertise it as a
// byte array. Keep all domain fields and references from the embedded OpenAPI,
// adapting only local file hashing and the server's optional-input semantics.
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

	// The input decoder defaults omitted booleans to false; the stored manifest
	// requires them explicitly. Do not insert defaults into the request because
	// its original JSON participates in the resumable state digest.
	rights := defs["VersionManifestRights"].(map[string]any)
	omitRequired(rights, "noai", "redistribute_raw")

	// Optional pointer/raw JSON inputs accept null just like REST decoding.
	// Omitted/null rights are resolved by the domain for an existing asset, and
	// rejected by the domain when creating an asset without declared rights.
	nullable := func(props map[string]any, names ...string) {
		for _, name := range names {
			props[name] = map[string]any{"anyOf": []any{props[name], map[string]any{"type": "null"}}}
		}
	}
	nullable(commit["properties"].(map[string]any), "task", "describe")
	patch := defs["AssetPatch"].(map[string]any)
	nullable(patch["properties"].(map[string]any), "title", "summary", "tags", "subjects", "extra", "sensitivity", "defaults")
	content := defs["ContentInput"].(map[string]any)
	nullable(content["properties"].(map[string]any), "rights", "metadata", "producer", "uses")
	return input, nil
}

// Remove only local-input exceptions, retaining any future domain requirements.
func omitRequired(schema map[string]any, names ...string) {
	schema["required"] = slices.DeleteFunc(schema["required"].([]any), func(value any) bool {
		name, ok := value.(string)
		return ok && slices.Contains(names, name)
	})
}
