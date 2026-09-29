package bridge

import (
	"log/slog"

	"github.com/df-mc/go-nethernet"
)

// NewHTTPSignaling returns the HTTP signaling that a NetherNet-speaking Bedrock server
// exposes (see httpSignaling), for callers outside this package that bring their own
// nethernet.Dialer - such as cmd/cbedit, which joins through gophertunnel so the join
// carries a real Xbox Live identity. Dialers using it must set DisableTrickleICE.
func NewHTTPSignaling(log *slog.Logger) nethernet.Signaling {
	return newHTTPSignaling(log, nil)
}
