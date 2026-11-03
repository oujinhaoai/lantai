package cli

import (
	"context"

	"github.com/oujinhaoai/lantai/internal/client"
)

func executeDerivation(ctx context.Context, o assetOptions) (any, error) {
	if o.output == "" || o.artifact == "" || o.directory == "" {
		return nil, usage("derive requires --directory, private --artifact and --output")
	}
	var input client.DerivationInput
	if err := decodeAssetInput(o.input, &input); err != nil {
		return nil, err
	}
	output, err := client.OpenBatchPrivateOutput(o.artifact, o.directory, o.input, o.output)
	if err != nil {
		return nil, err
	}
	defer output.Close()
	c, err := assetClient(o)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	receipt, err := c.DeriveTo(ctx, input, o.directory, output)
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
