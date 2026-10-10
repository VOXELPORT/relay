package main

// Permanent record of everything the dashboard shows, under -data-dir
// (/var/lib/voxelport-admin). Nothing is ever deleted; finished days are
// gzipped. All files are JSON Lines, one record per line:
//
//	metrics/YYYY-MM-DD.jsonl(.gz)  every 5-second sample
//	minutes/YYYY-MM.jsonl          1-minute averages (charts up to 7 days)
//	hourly.jsonl                   1-hour averages (charts beyond 7 days)
//	sessions.jsonl                 one line per finished server session
//	events.jsonl                   servers on/off, addresses, services, admin actions
//	downloads.jsonl                daily snapshot of download counts
//
// Days are cut at midnight in -tz (default Asia/Kolkata).

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type store struct {
	dir string
	loc *time.Location

	mu       sync.Mutex
	rawDay   string
	raw      *os.File
	hourKey  int64
	hourAcc  []sample
	lastSnap string // day of the last downloads snapshot
}

func openStore(dir string, loc *time.Location) (*store, error) {
	for _, sub := range []string{"", "metrics", "minutes"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, err
		}
	}
	st := &store{dir: dir, loc: loc}
	today := time.Now().In(loc).Format("2006-01-02")
	// Compress any finished day left uncompressed by a restart.
	files, _ := filepath.Glob(filepath.Join(dir, "metrics", "*.jsonl"))
	for _, f := range files {
		if !strings.HasPrefix(filepath.Base(f), today) {
			go gzipFile(f)
		}
	}
	st.lastSnap = st.lastDownloadsDay()
	return st, nil
}

func (st *store) day(t time.Time) string { return t.In(st.loc).Format("2006-01-02") }

func appendLine(path string, v any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// addRaw appends a 5-second sample to today's metrics file.
func (st *store) addRaw(p sample) {
	st.mu.Lock()
	defer st.mu.Unlock()
	day := st.day(time.UnixMilli(p.T))
	if day != st.rawDay || st.raw == nil {
		if st.raw != nil {
			prev := st.raw.Name()
			st.raw.Close()
			go gzipFile(prev)
		}
		f, err := os.OpenFile(filepath.Join(st.dir, "metrics", day+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			log.Printf("store: %v", err)
			st.raw = nil
			return
		}
		st.raw, st.rawDay = f, day
	}
	b, _ := json.Marshal(p)
	if _, err := st.raw.Write(append(b, '\n')); err != nil {
		log.Printf("store: %v", err)
	}
}

// addMinute appends a 1-minute average and rolls hours into hourly.jsonl.
func (st *store) addMinute(p sample) {
	t := time.UnixMilli(p.T).In(st.loc)
	if err := appendLine(filepath.Join(st.dir, "minutes", t.Format("2006-01")+".jsonl"), p); err != nil {
		log.Printf("store: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	hour := t.Truncate(time.Hour).Unix()
	if hour != st.hourKey && len(st.hourAcc) > 0 {
		avg := average(st.hourAcc)
		avg.T = time.Unix(st.hourKey, 0).Add(time.Hour).UnixMilli()
		if err := appendLine(filepath.Join(st.dir, "hourly.jsonl"), avg); err != nil {
			log.Printf("store: %v", err)
		}
		st.hourAcc = st.hourAcc[:0]
	}
	st.hourKey = hour
	st.hourAcc = append(st.hourAcc, p)
}

func (st *store) record(name string, v any) {
	if err := appendLine(filepath.Join(st.dir, name), v); err != nil {
		log.Printf("store: %v", err)
	}
}

// event writes one line to events.jsonl.
func (st *store) event(kind string, fields map[string]any) {
	e := map[string]any{"t": time.Now().UnixMilli(), "type": kind}
	for k, v := range fields {
		e[k] = v
	}
	st.record("events.jsonl", e)
}

func gzipFile(path string) {
	in, err := os.Open(path)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.OpenFile(path+".gz.tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return
	}
	zw := gzip.NewWriter(out)
	_, err = io.Copy(zw, in)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path + ".gz.tmp")
		log.Printf("store: gzip %s: %v", path, err)
		return
	}
	if os.Rename(path+".gz.tmp", path+".gz") == nil {
		os.Remove(path)
	}
}

// eachLine calls fn for every line of a .jsonl or .jsonl.gz file.
func eachLine(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return
		}
		defer zr.Close()
		r = zr
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			fn(sc.Bytes())
		}
	}
}

func readSamples(paths []string, from, to time.Time) []sample {
	out := []sample{}
	f, t := from.UnixMilli(), to.UnixMilli()
	for _, p := range paths {
		eachLine(p, func(b []byte) {
			var s sample
			if json.Unmarshal(b, &s) == nil && s.T >= f && s.T <= t {
				out = append(out, s)
			}
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

// rawPaths lists the 5-second files covering [from, to].
func (st *store) rawPaths(from, to time.Time) []string {
	var paths []string
	for d := from.In(st.loc); !d.After(to.In(st.loc).Add(24 * time.Hour)); d = d.AddDate(0, 0, 1) {
		base := filepath.Join(st.dir, "metrics", d.Format("2006-01-02")+".jsonl")
		if _, err := os.Stat(base); err == nil {
			paths = append(paths, base)
		} else if _, err := os.Stat(base + ".gz"); err == nil {
			paths = append(paths, base+".gz")
		}
	}
	return paths
}

func (st *store) minutePaths(from, to time.Time) []string {
	var paths []string
	start := time.Date(from.In(st.loc).Year(), from.In(st.loc).Month(), 1, 0, 0, 0, 0, st.loc)
	for m := start; !m.After(to); m = m.AddDate(0, 1, 0) {
		p := filepath.Join(st.dir, "minutes", m.Format("2006-01")+".jsonl")
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	return paths
}

func (st *store) raw5s(from, to time.Time) []sample {
	return readSamples(st.rawPaths(from, to), from, to)
}
func (st *store) minutes(from, to time.Time) []sample {
	return readSamples(st.minutePaths(from, to), from, to)
}
func (st *store) hourly(from, to time.Time) []sample {
	return readSamples([]string{filepath.Join(st.dir, "hourly.jsonl")}, from, to)
}

// downsample averages pts into at most n buckets so long ranges stay light.
func downsample(pts []sample, n int) []sample {
	if len(pts) <= n {
		return pts
	}
	out := make([]sample, 0, n)
	size := float64(len(pts)) / float64(n)
	for i := 0; i < n; i++ {
		a, b := int(float64(i)*size), int(float64(i+1)*size)
		if b > len(pts) {
			b = len(pts)
		}
		if a < b {
			out = append(out, average(pts[a:b]))
		}
	}
	return out
}

// tail returns the last n records of a JSONL file, newest first.
func (st *store) tail(name string, n int) []json.RawMessage {
	var all []json.RawMessage
	eachLine(filepath.Join(st.dir, name), func(b []byte) {
		all = append(all, append(json.RawMessage(nil), b...))
		if len(all) > n*4 {
			all = all[len(all)-n:]
		}
	})
	if len(all) > n {
		all = all[len(all)-n:]
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all
}

// records returns every record of a JSONL file whose "t" (unix ms) is in range.
func (st *store) records(name string, from, to time.Time) []map[string]any {
	out := []map[string]any{}
	eachLine(filepath.Join(st.dir, name), func(b []byte) {
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return
		}
		if t, ok := m["t"].(float64); ok && (int64(t) < from.UnixMilli() || int64(t) > to.UnixMilli()) {
			return
		}
		out = append(out, m)
	})
	return out
}

// usage reports how much is stored and since when.
func (st *store) usage() map[string]any {
	var size int64
	files := 0
	filepath.WalkDir(st.dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
				files++
			}
		}
		return nil
	})
	days, _ := filepath.Glob(filepath.Join(st.dir, "metrics", "*.jsonl*"))
	sort.Strings(days)
	since := ""
	if len(days) > 0 {
		since = strings.SplitN(filepath.Base(days[0]), ".", 2)[0]
	}
	return map[string]any{"bytes": size, "files": files, "since": since, "days": len(days), "dir": st.dir}
}

func (st *store) lastDownloadsDay() string {
	last := st.tail("downloads.jsonl", 1)
	if len(last) == 0 {
		return ""
	}
	var m struct {
		Day string `json:"day"`
	}
	json.Unmarshal(last[0], &m)
	return m.Day
}

// snapshotDownloads writes today's download counts once per day.
func (st *store) snapshotDownloads(v map[string]any) {
	day := st.day(time.Now())
	st.mu.Lock()
	if st.lastSnap == day {
		st.mu.Unlock()
		return
	}
	st.lastSnap = day
	st.mu.Unlock()
	rec := map[string]any{"t": time.Now().UnixMilli(), "day": day}
	for k, val := range v {
		rec[k] = val
	}
	st.record("downloads.jsonl", rec)
}
