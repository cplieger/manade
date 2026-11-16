// Package hostlink is the channel between the control plane and its hosts:
// a host dials the control plane, upgrades one HTTP/1.1 request, and then
// serves the host API over HTTP/2 on that connection while the control
// plane calls it.
package hostlink

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/config"
)

// protocol is the upgrade token, naming the host API's major.
const protocol = "manade-host/1"

// connectPath is the one route a host calls before the upgrade.
const connectPath = "/connect"

// The headers of the upgrade request.
const (
	headerName     = "Manade-Host"
	headerBuild    = "Manade-Build"
	headerInstance = "Manade-Instance"
	headerSection  = "Manade-Section"
)

// pingTimeout is both sides' HTTP/2 SendPingTimeout and PingTimeout: a peer
// that stops answering is dropped 20 seconds after its last frame.
const pingTimeout = 10 * time.Second

// handshakeTimeout bounds a dial and the upgrade's request and answer.
const handshakeTimeout = 10 * time.Second

// refusalLimit caps a refusal's body as either side reads it.
const refusalLimit = 4 << 10

// Hello is what a host says about itself in its upgrade request.
type Hello struct {
	// Name is the host's name, MANADE_HOST.
	Name string
	// Build is the host's build version.
	Build string
	// Instance identifies one run of the host.
	Instance string
	// Section is the revision of the section the host last applied, empty
	// before its first.
	Section string
}

func (h *Hello) header(hd http.Header) {
	hd.Set(headerName, h.Name)
	hd.Set(headerBuild, h.Build)
	hd.Set(headerInstance, h.Instance)
	if h.Section != "" {
		hd.Set(headerSection, h.Section)
	}
}

// helloFrom reads a host's Hello, refusing a name no host may have. Every
// field is the peer's text, which a log line tags with textsafe.NewUntrusted.
func helloFrom(hd http.Header) (Hello, error) {
	h := Hello{
		Name: hd.Get(headerName), Build: hd.Get(headerBuild),
		Instance: hd.Get(headerInstance), Section: hd.Get(headerSection),
	}
	if err := config.CheckHostName(h.Name); err != nil {
		return Hello{}, fmt.Errorf("%s: %w", headerName, err)
	}
	return h, nil
}

// The codes a refusal of the upgrade carries.
const (
	codeUpgradeRequired = "upgrade_required"
	codeBadRequest      = "bad_request"
)

// refusal is every answer to /connect but its 101.
type refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Want is the protocol the control plane speaks, on upgrade_required.
	Want string `json:"want,omitempty"`
	// Got is the protocol the host asked for, on upgrade_required.
	Got string `json:"got,omitempty"`
}

func writeRefusal(w http.ResponseWriter, status int, r *refusal) {
	b, _ := json.Marshal(r)
	w.Header().Set("Content-Type", "application/json")
	if r.Code == codeUpgradeRequired {
		// RFC 9110 §15.5.22: a 426 names the protocol to upgrade to.
		w.Header().Set("Upgrade", protocol)
		w.Header().Set("Connection", "Upgrade")
	}
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// refusedError is the control plane's refusal of a host's upgrade.
type refusedError struct {
	Code    string
	Message string
	Want    string
	Got     string
	Status  int
}

func (e *refusedError) Error() string {
	return fmt.Sprintf("the control plane refused the connection: status %d (%s): %s", e.Status, e.Code, e.Message)
}

func refusedFrom(status int, body []byte) *refusedError {
	var r refusal
	if json.Unmarshal(body, &r) != nil || r.Code == "" {
		return &refusedError{Status: status, Code: "unknown", Message: textsafe.SanitizeSingleLineBounded(string(body), 256)}
	}
	return &refusedError{
		Status: status, Code: textsafe.SanitizeSingleLineBounded(r.Code, 64), Message: textsafe.SanitizeSingleLineBounded(r.Message, 512),
		Want: textsafe.SanitizeSingleLineBounded(r.Want, 64), Got: textsafe.SanitizeSingleLineBounded(r.Got, 64),
	}
}
