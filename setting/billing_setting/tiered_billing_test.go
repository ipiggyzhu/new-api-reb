package billing_setting

import (
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBillingSettingsReadConsistentSnapshotDuringUpdates(t *testing.T) {
	saved, err := config.ConfigToMap(&billingSetting)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, config.UpdateConfigFromMap(&billingSetting, saved)) })
	versions := []map[string]string{
		{"billing_mode": `{"model":"ratio"}`, "billing_expr": `{"model":"p * 2"}`},
		{"billing_mode": `{"model":"tiered_expr"}`, "billing_expr": `{"model":"p * 3"}`},
	}
	require.NoError(t, config.UpdateConfigFromMap(&billingSetting, versions[0]))
	start := make(chan struct{})
	done := make(chan struct{})
	errors := make(chan error, 3)
	var readers sync.WaitGroup
	for i := 0; i < 2; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				data := GetPricingSyncData(map[string]any{"source": "local"})
				mode := data[BillingModeField].(map[string]string)["model"]
				expr := data[BillingExprField].(map[string]string)["model"]
				if (mode == "ratio" && expr != "p * 2") || (mode == "tiered_expr" && expr != "p * 3") {
					errors <- fmt.Errorf("mixed billing snapshot: mode=%s expr=%s", mode, expr)
					return
				}
				// Exercise the request hot-path readers alongside a live config save.
				GetBillingMode("model")
				GetBillingExpr("model")
			}
		}()
	}
	close(start)
	for i := 0; i < 100; i++ {
		if err := config.UpdateConfigFromMap(&billingSetting, versions[i%2]); err != nil {
			errors <- err
			break
		}
	}
	close(done)
	readers.Wait()
	close(errors)
	for err := range errors {
		assert.NoError(t, err)
	}
	data := GetPricingSyncData(map[string]any{"source": "local"})
	assert.Equal(t, "local", data["source"])
	data[BillingModeField].(map[string]string)["model"] = "modified"
	assert.Equal(t, BillingModeTieredExpr, GetBillingMode("model"))
}
