package images

import (
	"strings"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// pinned reports a digest reference, whose image cannot move.
func pinned(ref string) bool { return strings.Contains(ref, "@") }

// repository answers ref's repository, without tag or digest, in the full
// form normalize gives.
func repository(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}
	return normalize(name)
}

// normalize answers a repository name in Docker's full form, which the
// daemon shortens for Docker Hub in RepoTags and RepoDigests: "ubuntu" and
// "docker.io/library/ubuntu" are one repository
// (https://github.com/distribution/reference/blob/main/normalize.go).
func normalize(name string) string {
	if reg, _ := config.Registry(name); reg != "" {
		return name
	}
	if first, rest, ok := strings.Cut(name, "/"); ok && strings.ContainsAny(first, ".:") {
		name = rest
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return "docker.io/" + name
}

// digestOf answers the digest img was pulled under for ref's repository, or
// "" when it has none there: an image loaded rather than pulled.
func digestOf(ref string, img *docker.Image) string {
	repo := repository(ref)
	for _, d := range img.RepoDigests {
		if name, digest, ok := strings.Cut(d, "@"); ok && normalize(name) == repo {
			return digest
		}
	}
	return ""
}
