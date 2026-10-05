package gateway

import (
	"path/filepath"
	"testing"

	"github.com/phin-tech/ignatius/go/ignatius"
)

// What the asynchronous store write adds to a request (SPEC 13.1):
//
//	go test ./gateway -run xxx -bench StoreOverhead -benchmem
func benchStore(b *testing.B, withStore, content bool) {
	up := newFake(b, choiceSure)
	cfg := Config{Models: map[string]ignatius.ModelConfig{"m": {Provider: "systemone", BaseURL: up.URL, Model: "jev-latest"}}}
	if withStore {
		cfg.Store = &StoreConfig{}
		cfg.Clients = []ClientConfig{{Name: "a", KeyEnv: "KA", StoreContent: content}}
	}
	cfg.applyDefaults()
	env := map[string]string{"KA": "ka", "IGNATIUS_STORE_DSN": filepath.Join(b.TempDir(), "s.db")}
	getenv := func(k string) string { return env[k] }
	reg, _ := ignatius.BuildRegistry(cfg.Models, getenv)
	s, err := New(cfg, reg, getenv)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	body := choiceReq("m", "a ticket about a duplicate charge")
	auth := ""
	if withStore {
		auth = "Bearer ka"
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rec := do(s, "POST", "/v1/systemone", body, auth); rec.Code != 200 {
			b.Fatal(rec.Code, rec.Body)
		}
	}
	if s.writer != nil { // writes are asynchronous: say how many the bounded queue dropped
		b.StopTimer()
		b.ReportMetric(float64(s.writer.dropped.Load())/float64(b.N), "dropped/op")
	}
}

func BenchmarkStoreOverhead(b *testing.B) {
	b.Run("no store", func(b *testing.B) { benchStore(b, false, false) })
	b.Run("metadata only", func(b *testing.B) { benchStore(b, true, false) })
	b.Run("with content", func(b *testing.B) { benchStore(b, true, true) })
}
