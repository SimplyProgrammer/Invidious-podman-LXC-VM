package adaptive

import (
	"errors"
	"testing"
	"time"
)

func testPool(t *testing.T, configs []BackendConfig, maxPenalty *float64) *BackendPool {
	t.Helper()

	pool, err := newBackendPool(
		configs,
		maxPenalty,
		time.Hour,
		1,
	)
	if err != nil {
		t.Fatalf("newBackendPool: %v", err)
	}

	t.Cleanup(pool.Close)
	return pool
}

func TestSelectExcludesPreviouslyAttemptedBackend(t *testing.T) {
	pool := testPool(t, []BackendConfig{
		{Name: "local", URL: "http://local:3000", Penalty: 0},
		{Name: "remote", URL: "https://remote.example", Penalty: 1},
	}, nil)

	selected, err := pool.Select(map[string]struct{}{
		"local": {},
	})
	if err != nil {
		t.Fatal(err)
	}

	if selected.Name != "remote" {
		t.Fatalf("selected %q; want remote", selected.Name)
	}
}

func TestSelectReturnsErrorWhenAllBackendsExcluded(t *testing.T) {
	pool := testPool(t, []BackendConfig{
		{Name: "one", URL: "http://one:3000"},
	}, nil)

	_, err := pool.Select(map[string]struct{}{
		"one": {},
	})

	if !errors.Is(err, ErrNoEligibleBackend) {
		t.Fatalf("got %v; want ErrNoEligibleBackend", err)
	}
}

func TestPenaltyIncreaseAndRecovery(t *testing.T) {
	pool := testPool(t, []BackendConfig{
		{Name: "local", URL: "http://local:3000", Penalty: 0},
		{Name: "remote", URL: "https://remote.example", Penalty: 1},
	}, nil)

	if err := pool.AddPenalty("local", 2); err != nil {
		t.Fatal(err)
	}
	if err := pool.AddPenalty("remote", 2); err != nil {
		t.Fatal(err)
	}

	pool.RecoverNow()

	s := pool.Snapshot()
	if s[0].Penalty != 1 || s[1].Penalty != 2 {
		t.Fatalf("unexpected penalties after recovery: %+v", s)
	}

	pool.RecoverNow()

	s = pool.Snapshot()
	if s[0].Penalty != 0 || s[1].Penalty != 1 {
		t.Fatalf("unexpected penalties after second recovery: %+v", s)
	}

	// Recovery must not erase configured starting preferences.
	pool.RecoverNow()

	s = pool.Snapshot()
	if s[0].Penalty != 0 || s[1].Penalty != 1 {
		t.Fatalf("penalty fell below initial values: %+v", s)
	}
}

func TestMaxPenaltyCapsAccumulation(t *testing.T) {
	capValue := 4.0

	pool := testPool(t, []BackendConfig{
		{Name: "remote", URL: "https://remote.example", Penalty: 1},
	}, &capValue)

	if err := pool.AddPenalty("remote", 20); err != nil {
		t.Fatal(err)
	}

	if got := pool.Snapshot()[0].Penalty; got != 4 {
		t.Fatalf("penalty=%v; want 4", got)
	}
}

func TestMaxPenaltyCannotBeBelowInitialPenalty(t *testing.T) {
	capValue := 0.0

	_, err := newBackendPool(
		[]BackendConfig{
			{Name: "remote", URL: "https://remote.example", Penalty: 1},
		},
		&capValue,
		time.Hour,
		1,
	)

	if err == nil {
		t.Fatal("expected error")
	}
}

func TestUnknownBackendPenalty(t *testing.T) {
	pool := testPool(t, []BackendConfig{
		{Name: "local", URL: "http://local:3000"},
	}, nil)

	if err := pool.AddPenalty("missing", 1); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("got %v; want ErrUnknownBackend", err)
	}
}

func TestPenaltyWeights(t *testing.T) {
	tests := []struct {
		penalty float64
		want    float64
	}{
		{penalty: 0, want: 1.0},
		{penalty: 1, want: 0.5},
		{penalty: 3, want: 0.25},
	}

	for _, test := range tests {
		got := penaltyWeight(test.penalty)
		if got != test.want {
			t.Errorf(
				"penaltyWeight(%v) = %v; want %v",
				test.penalty, got, test.want,
			)
		}
	}
}

func TestFractionalPenaltyIncreaseAndRecovery(t *testing.T) {
	recovery := 0.25
	pool, err := newBackendPool(
		[]BackendConfig{{Name: "remote", URL: "https://remote.example", Penalty: 0.5}},
		nil,
		time.Hour,
		recovery,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := pool.AddPenalty("remote", 1.25); err != nil {
		t.Fatal(err)
	}
	if got := pool.Snapshot()[0].Penalty; got != 1.75 {
		t.Fatalf("penalty=%v; want 1.75", got)
	}
	pool.RecoverNow()
	if got := pool.Snapshot()[0].Penalty; got != 1.5 {
		t.Fatalf("penalty after fractional recovery=%v; want 1.5", got)
	}
	pool.RecoverNow()
	pool.RecoverNow()
	pool.RecoverNow()
	pool.RecoverNow()
	if got := pool.Snapshot()[0].Penalty; got != 0.5 {
		t.Fatalf("penalty after recovery=%v; want initial 0.5", got)
	}
}

func TestFractionalMaxPenalty(t *testing.T) {
	capValue := 2.75
	pool := testPool(t, []BackendConfig{
		{Name: "remote", URL: "https://remote.example", Penalty: 0.5},
	}, &capValue)
	if err := pool.AddPenalty("remote", 5.125); err != nil {
		t.Fatal(err)
	}
	if got := pool.Snapshot()[0].Penalty; got != 2.75 {
		t.Fatalf("penalty=%v; want cap 2.75", got)
	}
}
