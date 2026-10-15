package reth

import "testing"

func TestReserveFor_TablePoints(t *testing.T) {
	for _, pt := range fillReservePoints {
		got, err := reserveFor(pt.fill)
		if err != nil {
			t.Fatalf("reserveFor(%v): %v", pt.fill, err)
		}
		if got != pt.reserve {
			t.Errorf("reserveFor(%v) = %d, want %d", pt.fill, got, pt.reserve)
		}
	}
}

func TestReserveFor_Midpoints(t *testing.T) {
	cases := []struct {
		fill    float64
		want    uint64
		comment string
	}{
		// Midpoint between (0, 0.988) and (8192, 0.863): fill=0.9255.
		{0.9255, 4096, "between reserve 0 and 8192"},
		// Midpoint between (8192, 0.863) and (16384, 0.738): fill=0.8005.
		{0.8005, 12288, "between reserve 8192 and 16384"},
		// Above the densest measured point: clamps to reserve 0.
		{0.995, 0, "above densest point clamps to 0"},
		// Below the sparsest measured point (but still > 0.5): clamps to 32768.
		{0.503, 32768, "below sparsest point clamps to 32768"},
	}
	for _, c := range cases {
		got, err := reserveFor(c.fill)
		if err != nil {
			t.Fatalf("reserveFor(%v) [%s]: %v", c.fill, c.comment, err)
		}
		if got != c.want {
			t.Errorf("reserveFor(%v) [%s] = %d, want %d", c.fill, c.comment, got, c.want)
		}
	}
}

func TestReserveFor_RejectsOutOfRange(t *testing.T) {
	for _, f := range []float64{0, 0.5, 1.001, -0.1, 2.0} {
		if _, err := reserveFor(f); err == nil {
			t.Errorf("reserveFor(%v): want error, got nil", f)
		}
	}
	// 1.0 itself is the inclusive upper bound.
	if got, err := reserveFor(1.0); err != nil || got != 0 {
		t.Errorf("reserveFor(1.0) = %d, %v; want 0, nil", got, err)
	}
}

func TestParseFillOverrides(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := ParseFillOverrides("")
		if err != nil || len(got) != 0 {
			t.Fatalf("ParseFillOverrides(\"\") = %v, %v; want empty map, nil", got, err)
		}
	})
	t.Run("single", func(t *testing.T) {
		got, err := ParseFillOverrides("HashedAccounts=0.59")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["HashedAccounts"] != 0.59 || len(got) != 1 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("multiple", func(t *testing.T) {
		got, err := ParseFillOverrides("HashedAccounts=0.59,HashedStorages=0.72")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["HashedAccounts"] != 0.59 || got["HashedStorages"] != 0.72 || len(got) != 2 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("invalid table", func(t *testing.T) {
		if _, err := ParseFillOverrides("NotATable=0.6"); err == nil {
			t.Fatal("want error for unrecognized table")
		}
	})
	t.Run("invalid value", func(t *testing.T) {
		if _, err := ParseFillOverrides("HashedAccounts=notafloat"); err == nil {
			t.Fatal("want error for non-numeric value")
		}
	})
	t.Run("value out of range", func(t *testing.T) {
		if _, err := ParseFillOverrides("HashedAccounts=0.3"); err == nil {
			t.Fatal("want error for value <= 0.5")
		}
		if _, err := ParseFillOverrides("HashedAccounts=1.5"); err == nil {
			t.Fatal("want error for value > 1.0")
		}
	})
	t.Run("nan", func(t *testing.T) {
		if _, err := ParseFillOverrides("HashedAccounts=NaN"); err == nil {
			t.Fatal("want error for NaN value")
		}
	})
	t.Run("malformed entry", func(t *testing.T) {
		if _, err := ParseFillOverrides("HashedAccounts"); err == nil {
			t.Fatal("want error for entry missing '='")
		}
	})
}

func TestResolveFillProfile(t *testing.T) {
	t.Run("fresh is dense", func(t *testing.T) {
		got, err := ResolveFillProfile("fresh", nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("ResolveFillProfile(fresh) = %v, %v; want empty map, nil", got, err)
		}
	})
	t.Run("override wins over profile base", func(t *testing.T) {
		got, err := ResolveFillProfile("migrated", map[string]float64{"HashedAccounts": 0.8})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["HashedAccounts"] != 0.8 {
			t.Fatalf("override did not win: %v", got)
		}
	})
	t.Run("unknown profile rejected", func(t *testing.T) {
		if _, err := ResolveFillProfile("ancient", nil); err == nil {
			t.Fatal("want error for unrecognized profile")
		}
	})
}
