package llmgateway

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"regexp"
)

var sessionPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

// The label is client-supplied correlation, never authorization. Hashing it
// together with the verified principal isolates identical labels across apps
// and avoids forwarding arbitrary client text to a provider or the ledger.
func conversationOf(r *http.Request, c Caller) (string, error) {
	values := r.Header.Values("X-Station-Session")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || !sessionPattern.MatchString(values[0]) {
		return "", fmt.Errorf("X-Station-Session must be one opaque ID of 1-128 letters, digits, dots, underscores, colons or hyphens")
	}
	if !c.Known {
		return "", fmt.Errorf("conversation identity requires an authenticated principal")
	}
	sum := sha256.Sum256([]byte(c.ID + "\x00" + values[0]))
	return fmt.Sprintf("station-%x", sum[:]), nil
}
