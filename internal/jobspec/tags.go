package jobspec

import (
	"regexp"
	"strconv"

	"github.com/cplieger/manade/internal/config"
)

// RunnerRepo is the package the shipped runner images are published in.
const RunnerRepo = "ghcr.io/cplieger/manade-runner"

// moduleMajor is the major of a build whose version is not a release tag: the
// module path's, which has no /vN suffix.
const moduleMajor = 1

// releaseRe is a release tag, its major and its prerelease.
var releaseRe = regexp.MustCompile(`^v(\d{1,6})\.\d{1,6}\.\d{1,6}(-[0-9A-Za-z.-]+)?$`)

// Channel answers the major and channel of a build of version: a release
// tag's own major, stable only without a prerelease; any other version
// (dev, ci) is moduleMajor on the dev channel.
func Channel(version string) (major int, dev bool) {
	m := releaseRe.FindStringSubmatch(version)
	if m == nil {
		return moduleMajor, true
	}
	n, _ := strconv.Atoi(m[1]) // at most six digits
	return n, m[2] != ""
}

// ImageRef is the reference profile p's image is pulled by on a host built
// at version: its image field, or the shipped image's moving tag of the
// host's major and channel, so a host never follows a job image of a
// contract it cannot run.
func ImageRef(p *config.Profile, version string) string {
	if p.Image != "" {
		return p.Image
	}
	major, dev := Channel(version)
	tag := p.RunnerImage + "-v" + strconv.Itoa(major)
	if dev {
		tag += "-dev"
	}
	return RunnerRepo + ":" + tag
}
