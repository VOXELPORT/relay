package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *store {
	t.Helper()
	loc, _ := time.LoadLocation("Asia/Kolkata")
	st, err := openStore(t.TempDir(), loc)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStoreRoundTripAndGzip(t *testing.T) {
	st := testStore(t)
	day1 := time.Date(2026, 10, 1, 12, 0, 0, 0, st.loc)
	day2 := day1.AddDate(0, 0, 1)
	st.addRaw(sample{T: day1.UnixMilli(), CPU: 10, Players: 3})
	st.addRaw(sample{T: day2.UnixMilli(), CPU: 20, Players: 5}) // rolls over and gzips day 1
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(st.dir, "metrics", "2026-10-01.jsonl.gz")); err != nil {
		t.Fatalf("finished day not gzipped: %v", err)
	}
	got := st.raw5s(day1.Add(-time.Hour), day2.Add(time.Hour))
	if len(got) != 2 || got[0].CPU != 10 || got[1].Players != 5 {
		t.Fatalf("round trip: %+v", got)
	}
	if u := st.usage(); u["since"] != "2026-10-01" {
		t.Fatalf("usage since: %v", u["since"])
	}
}

func TestMinutesRollIntoHourly(t *testing.T) {
	st := testStore(t)
	base := time.Date(2026, 10, 1, 10, 0, 0, 0, st.loc)
	for i := 0; i < 61; i++ { // 10:00 … 11:00 — the 11:00 minute closes hour 10
		st.addMinute(sample{T: base.Add(time.Duration(i) * time.Minute).UnixMilli(), Players: float64(i % 2 * 2)})
	}
	if n := len(st.minutes(base, base.Add(2*time.Hour))); n != 61 {
		t.Fatalf("minutes: %d", n)
	}
	h := st.hourly(base, base.Add(3*time.Hour))
	if len(h) != 1 || h[0].Players != 1 {
		t.Fatalf("hourly: %+v", h)
	}
}

func relaySnapshot(players ...string) map[string]any {
	ps := []any{}
	for _, p := range players {
		ps = append(ps, p)
	}
	return map[string]any{
		"public_host": "play.example.test",
		"tunnels": []any{map[string]any{
			"id": "abcdef123456", "port": 26001.0, "name": "traz", "since": "2026-10-10T10:00:00Z",
			"players": ps, "udp": true, "udp_sessions": 1.0, "bytes_in": 100.0, "bytes_out": 900.0, "conns": 4.0, "peak_players": 2.0,
			"listing": map[string]any{"title": "=HYPERLINK(\"x\")", "mode": "survival", "version": "26.2"},
		}},
		"names": []any{map[string]any{"name": "traz", "id": "abcdef123456", "port": 26001.0}},
	}
}

func TestTrackerRecordsSessionsWithoutIPs(t *testing.T) {
	st := testStore(t)
	tr := newTracker(st)
	tr.observe(relaySnapshot("203.0.113.5"), nil, nil, map[string]string{"voxelport-relay": "active"})
	tr.observe(relaySnapshot("203.0.113.5", "198.51.100.7"), nil, nil, map[string]string{"voxelport-relay": "active"})
	tr.observe(map[string]any{"public_host": "play.example.test", "tunnels": []any{}, "names": []any{}}, nil, nil, map[string]string{"voxelport-relay": "failed"})

	recs := st.records("sessions.jsonl", time.Unix(0, 0), time.Now().Add(time.Minute))
	if len(recs) != 1 {
		t.Fatalf("sessions: %+v", recs)
	}
	s := recs[0]
	if s["address"] != "traz.example.test" || s["unique_players"] != 2.0 || s["peak_players"] != 3.0 || s["bytes_out"] != 900.0 {
		t.Fatalf("session: %+v", s)
	}
	for _, name := range []string{"sessions.jsonl", "events.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(st.dir, name))
		if strings.Contains(string(b), "203.0.113.5") || strings.Contains(string(b), "198.51.100.7") {
			t.Fatalf("%s contains a player IP: %s", name, b)
		}
	}
	types := map[string]bool{}
	for _, e := range st.records("events.jsonl", time.Unix(0, 0), time.Now().Add(time.Minute)) {
		types[e["type"].(string)] = true
	}
	for _, want := range []string{"server_offline", "address_released", "service"} {
		if !types[want] {
			t.Errorf("missing %s event; got %v", want, types)
		}
	}
}

func TestExportSessionsCSVIsFormulaSafe(t *testing.T) {
	st := testStore(t)
	tr := newTracker(st)
	tr.observe(relaySnapshot(), nil, nil, nil)
	tr.observe(map[string]any{"public_host": "play.example.test", "tunnels": []any{}, "names": []any{}}, nil, nil, nil)
	today := time.Now().In(st.loc).Format("2006-01-02")
	rec := httptest.NewRecorder()
	exportData(rec, httptest.NewRequest("GET", "/api/export?what=sessions&from="+today+"&to="+today, nil), st, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "unique_players") || !strings.Contains(body, `'=HYPERLINK`) {
		t.Fatalf("export %d:\n%s", rec.Code, body)
	}
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "voxelport-sessions-") {
		t.Fatalf("no attachment header")
	}
}

func TestDownsample(t *testing.T) {
	pts := make([]sample, 5000)
	for i := range pts {
		pts[i] = sample{T: int64(i), Players: 2}
	}
	out := downsample(pts, 1000)
	if len(out) != 1000 || out[0].Players != 2 {
		t.Fatalf("downsample: %d %+v", len(out), out[0])
	}
}
