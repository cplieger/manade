package host

import (
	"slices"
	"strconv"
	"strings"
)

// pins assigns whole CPUs of the host's effective cpuset to runners on a
// kernel without CFS quota. A runner holds its CPUs until its container has
// exited; the holder's lock guards it.
type pins struct {
	held map[string][]int
	cpus []int
}

func newPins(cpus []int) *pins {
	return &pins{cpus: slices.Clone(cpus), held: map[string][]int{}}
}

// take assigns n free CPUs to runner name, the lowest first, or answers
// false when fewer than n are free. A runner already holding CPUs keeps them.
func (p *pins) take(name string, n int) ([]int, bool) {
	if got, ok := p.held[name]; ok {
		return got, len(got) == n
	}
	used := map[int]bool{}
	for _, cs := range p.held {
		for _, c := range cs {
			used[c] = true
		}
	}
	var got []int
	for _, c := range p.cpus {
		if len(got) == n {
			break
		}
		if !used[c] {
			got = append(got, c)
		}
	}
	if len(got) < n {
		return nil, false
	}
	p.held[name] = got
	return got, true
}

// hold records CPUs a container of runner name was built with, read back
// from the daemon at a supervisor start.
func (p *pins) hold(name string, cpus []int) { p.held[name] = cpus }

func (p *pins) release(name string) { delete(p.held, name) }

// free counts the CPUs no runner holds.
func (p *pins) free() int {
	n := len(p.cpus)
	for _, cs := range p.held {
		n -= len(cs)
	}
	return n
}

// formatCPUSet writes cpus as a container's CpusetCpus.
func formatCPUSet(cpus []int) string {
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = strconv.Itoa(c)
	}
	return strings.Join(s, ",")
}

// parseCPUSet reads a container's CpusetCpus as the host writes it: a
// comma-separated list of CPU numbers.
func parseCPUSet(s string) ([]int, bool) {
	if s == "" {
		return nil, false
	}
	var out []int
	for f := range strings.SplitSeq(s, ",") {
		c, err := strconv.Atoi(f)
		if err != nil || c < 0 {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
