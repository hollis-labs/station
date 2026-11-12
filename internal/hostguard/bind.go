package hostguard

import (
	"fmt"
	"net"
)

// RequireLoopbackBind refuses a listen address that is not a loopback IP (or "localhost").
// The framework's own bind check accepts every address in enforce mode, so keeping a Station
// unit off other interfaces is this rule's job, applied before anything is started or listened on.
func RequireLoopbackBind(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("station: httphost.addr %q is not host:port", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("station: httphost.addr %q is not a loopback address; Station listens on loopback only", addr)
}
