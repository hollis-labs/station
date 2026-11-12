package hostguard

import "testing"

func TestRequireLoopbackBind(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8085": true, "[::1]:8085": true, "localhost:8085": true, "127.0.0.1:0": true,
		":8085": false, "0.0.0.0:8085": false, "[::]:8085": false, "192.0.2.5:8085": false,
		"100.64." + "0.5:8085": false, "example.test:8085": false, "8085": false, "": false,
	} {
		if err := RequireLoopbackBind(addr); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", addr, err, ok)
		}
	}
}
