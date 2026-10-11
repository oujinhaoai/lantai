package mcpserver

// pullInputSchema retains the public read-grant purpose contract. Go reflection
// only sees a string and cannot advertise the enum needed to choose a valid use.
func (a *adapter) pullInputSchema() (map[string]any, error) {
	request := a.schema["components"].(map[string]any)["schemas"].(map[string]any)["ReadGrantRequest"].(map[string]any)
	input, err := a.localSchema(map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"directory", "asset_id", "version_id", "purpose"},
		"properties": map[string]any{
			"directory":  map[string]any{"type": "string"},
			"asset_id":   map[string]any{"type": "string"},
			"version_id": map[string]any{"type": "string"},
			"purpose":    request["properties"].(map[string]any)["purpose"],
		},
	})
	if err != nil {
		return nil, err
	}
	input["properties"].(map[string]any)["purpose"].(map[string]any)["description"] = "Intended use: select one enum value, not a free-text reason. The server still checks rights and authorization for that use."
	return input, nil
}
