package flow

import (
	"context"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
)

// CanonicalJSON delegates semantic identity to the shared core call boundary.
// Exact numbers and duplicate-key rejection therefore cannot drift in two layers.
func CanonicalJSON(ctx context.Context, input json.RawMessage) ([]byte, error) {
	return core.CanonicalFlowJSON(ctx, input)
}
