package integration

import (
	"testing"

	"github.com/dnaeon/gru/minion"
)

func TestMinionLastseen(t *testing.T) {
	tc := mustNewTestClient("fixtures/minion-lastseen")
	defer tc.recorder.Stop()

	cfg := &minion.EtcdMinionConfig{
		Name:       "Kevin",
		EtcdConfig: tc.config,
	}
	m, err := minion.NewEtcdMinion(cfg)
	if err != nil {
		t.Fatal(err)
	}

	id := m.ID()
	var want int64 = 1450357761

	err = m.SetLastseen(want)
	if err != nil {
		t.Fatal(err)
	}

	got, err := tc.client.MinionLastseen(id)
	if err != nil {
		t.Fatal(err)
	}

	if want != got {
		t.Errorf("want %d lastseen, got %d lastseen", want, got)
	}
}
