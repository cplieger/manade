package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/cplieger/manade/internal/config"
)

// PolicyHash answers the hash of the part of host section h its rules are
// built from: dns, egress and network.subnet. A host reports it, and a proof
// counts only while it is the running section's, so a change to a profile,
// images.refresh or capacity keeps the proof.
func PolicyHash(h *config.Host) string {
	return digest("manade policy\n", struct {
		DNS    any
		Subnet any
		Egress config.Egress
	}{h.DNS, h.Subnet, h.Egress})
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
