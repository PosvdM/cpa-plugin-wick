package config

import "testing"

func TestOtherClientsBoolean(t *testing.T) {
	for _, raw := range []string{"", "other_clients: true\n", "other_clients: false\n"} {
		cfg, err := Parse([]byte(raw))
		want := raw == "other_clients: true\n"
		if err != nil || cfg.OtherClients != want {
			t.Fatalf("%q: other_clients=%v err=%v", raw, cfg.OtherClients, err)
		}
	}
}
