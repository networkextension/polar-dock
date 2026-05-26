package dock

// Pure-logic tests for video_billing_store helpers. DB round-trip is
// covered by manual smoke (see doc/llm/video-billing.md §Verification).

import (
	"database/sql"
	"testing"
)

func TestNullFloat_Pointer(t *testing.T) {
	if got := nullFloat(nil); got.Valid {
		t.Fatalf("nil should be invalid")
	}
	v := 1.25
	got := nullFloat(&v)
	if !got.Valid || got.Float64 != 1.25 {
		t.Fatalf("want {1.25, valid}, got %+v", got)
	}
}

func TestNullInt_Pointer(t *testing.T) {
	if got := nullInt(nil); got.Valid {
		t.Fatalf("nil should be invalid")
	}
	v := 240
	got := nullInt(&v)
	if !got.Valid || got.Int64 != 240 {
		t.Fatalf("want {240, valid}, got %+v", got)
	}
}

// fakeScanner satisfies the `func(...interface{}) error` signature of
// row.Scan/rows.Scan. Lets us exercise scanVideoShotCall's null
// unwrapping without a real DB connection.
type fakeScanner struct {
	values []interface{}
}

func (f *fakeScanner) Scan(dest ...interface{}) error {
	for i, d := range dest {
		switch v := d.(type) {
		case *int64:
			*v = f.values[i].(int64)
		case *string:
			*v = f.values[i].(string)
		case *sql.NullFloat64:
			if f.values[i] == nil {
				*v = sql.NullFloat64{}
			} else {
				*v = sql.NullFloat64{Float64: f.values[i].(float64), Valid: true}
			}
		case *sql.NullInt64:
			if f.values[i] == nil {
				*v = sql.NullInt64{}
			} else {
				*v = sql.NullInt64{Int64: f.values[i].(int64), Valid: true}
			}
		case *[]byte:
			if f.values[i] == nil {
				*v = nil
			} else {
				*v = f.values[i].([]byte)
			}
		default:
			// time.Time uses *time.Time — leave zero.
		}
	}
	return nil
}

func TestScanVideoShotCall_NullableFields(t *testing.T) {
	// All-null nullable fields → pointers stay nil.
	f := &fakeScanner{values: []interface{}{
		int64(1), "ws-a", int64(10), int64(100), "video.seedance",
		"m", "720p",
		nil, nil, nil, // dur, fps, frames
		nil, nil, // cost, perFrame
		nil,                  // meta
		nil,                  // created_at (zero time)
	}}
	c, err := scanVideoShotCall(f.Scan)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != 1 || c.WorkspaceID != "ws-a" || c.Provider != "video.seedance" {
		t.Fatalf("primitive fields wrong: %+v", c)
	}
	if c.DurationChargedSec != nil || c.FPS != nil || c.FramesTotal != nil ||
		c.CostUSD != nil || c.CostPerFrameUSD != nil {
		t.Fatalf("nullables should be nil, got %+v", c)
	}
}

func TestScanVideoShotCall_PopulatedFields(t *testing.T) {
	f := &fakeScanner{values: []interface{}{
		int64(2), "ws-b", int64(11), int64(101), "video.seedance",
		"m", "1080p",
		10.0, int64(24), int64(240),
		0.50, 0.002083333,
		[]byte(`{"rate":0.05}`),
		nil,
	}}
	c, err := scanVideoShotCall(f.Scan)
	if err != nil {
		t.Fatal(err)
	}
	if c.DurationChargedSec == nil || *c.DurationChargedSec != 10.0 {
		t.Fatalf("dur not set: %+v", c)
	}
	if c.FPS == nil || *c.FPS != 24 {
		t.Fatalf("fps not set: %+v", c)
	}
	if c.FramesTotal == nil || *c.FramesTotal != 240 {
		t.Fatalf("frames not set: %+v", c)
	}
	if c.CostUSD == nil || *c.CostUSD != 0.50 {
		t.Fatalf("cost not set: %+v", c)
	}
	if string(c.BillingMeta) != `{"rate":0.05}` {
		t.Fatalf("meta not set: %s", c.BillingMeta)
	}
}
