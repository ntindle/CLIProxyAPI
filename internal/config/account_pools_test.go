package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestForkAccountPoolsConfigRoundTrip(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`client:
  account-pools:
    - name: reviewer
      auth-files: [reviewer.json]
      auth-kind: oauth
  key-scopes:
    - key-prefix: review-
      providers: [claude]
      pools: [reviewer]
    - key-prefix: deny-
      providers: [claude]
      pools: []
    - key-prefix: legacy-
      providers: [claude]
`))
	if err != nil {
		t.Fatal(err)
	}
	clone := cfg.CloneForRuntime()
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseConfigBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Client, reloaded.Client) {
		t.Fatal("pool config lost on round trip")
	}
	if cfg.Client.AccountPools == nil || (*cfg.Client.KeyScopes)[1].Pools == nil || (*cfg.Client.KeyScopes)[2].Pools != nil {
		t.Fatal("pool config or empty/omitted distinction lost")
	}
	(*cfg.Client.AccountPools)[0].AuthFiles[0] = "other.json"
	(*(*cfg.Client.KeyScopes)[0].Pools)[0] = "other"
	if (*clone.Client.AccountPools)[0].AuthFiles[0] != "reviewer.json" || (*(*clone.Client.KeyScopes)[0].Pools)[0] != "reviewer" {
		t.Fatal("runtime snapshot shares pool config")
	}
}
