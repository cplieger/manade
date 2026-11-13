package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/cplieger/manade/internal/config"
)

// PolicyHash answers the hash of host section h, every field dns included,
// which a host reports and the orchestrator checks against its own copy
// before it gives the host work.
func PolicyHash(h *config.Host) string {
	return digest("manade policy\n", h)
}

// ProfileHash answers the hash of the profiles that list host, in the
// order profiles holds them.
func ProfileHash(profiles []config.Profile, host string) string {
	docs := []*config.Profile{}
	for i := range profiles {
		if slices.Contains(profiles[i].Hosts, host) {
			docs = append(docs, &profiles[i])
		}
	}
	return digest("manade profiles\n", docs)
}

// digest hashes v's JSON, which encoding/json writes in field order, so a
// validated config hashes the same in every process.
func digest(kind string, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("egress: hashing a validated config: " + err.Error())
	}
	sum := sha256.Sum256(append([]byte(kind), b...))
	return hex.EncodeToString(sum[:])
}
