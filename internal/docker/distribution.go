package docker

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/cplieger/go/textsafe"
)

// digestRe is a sha256 digest as a registry answers it.
var digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// authHeader is the header a registry credential travels in.
const authHeader = "X-Registry-Auth"

// Digest asks the registry, through the daemon, for the digest ref's tag
// points at now: a manifest request, no pull. auth is an encoded
// X-Registry-Auth value, or "" for none.
func (c *Client) Digest(ctx context.Context, ref, auth string) (string, error) {
	var out struct {
		Descriptor struct {
			Digest string `json:"digest"`
		}
	}
	err := c.callAuth(ctx, "distribution", http.MethodGet, "/distribution/"+ref+"/json", auth, &out)
	if err != nil {
		return "", err
	}
	if !digestRe.MatchString(out.Descriptor.Digest) {
		return "", errors.New("docker distribution: the daemon answered digest " + textsafe.SanitizeSingleLineBounded(out.Descriptor.Digest, 80))
	}
	return out.Descriptor.Digest, nil
}
