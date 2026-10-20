package cmd

import (
	"context"
	"encoding/json"
	"io"

	"github.com/Songmu/ghalet"
)

func list(ctx context.Context, config string, out io.Writer) error {
	daemons, err := ghalet.ListDaemons(ctx, config)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Daemons []ghalet.DaemonInfo `json:"daemons"`
	}{Daemons: daemons})
}
