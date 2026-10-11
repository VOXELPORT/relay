package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

var sampleCols = []string{"time", "cpu_percent", "memory_used_bytes", "disk_used_bytes", "temperature_c", "load_1m",
	"internet_down_bytes_per_s", "internet_up_bytes_per_s", "relay_in_bytes_per_s", "relay_out_bytes_per_s",
	"players", "servers", "website_visitors_now",
	"na_relay_in_bytes_per_s", "na_relay_out_bytes_per_s", "na_players", "na_servers", "na_cpu_percent",
	"na_internet_down_bytes_per_s", "na_internet_up_bytes_per_s"}

func sampleRow(p sample, loc *time.Location) []string {
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	return []string{time.UnixMilli(p.T).In(loc).Format(time.RFC3339), f(p.CPU), f(p.MemUsed), f(p.DiskUsed), f(p.TempC), f(p.Load1),
		f(p.NetRx), f(p.NetTx), f(p.RelayIn), f(p.RelayOut), f(p.Players), f(p.Tunnels), f(p.WebLive),
		f(p.NAIn), f(p.NAOut), f(p.NAPlayers), f(p.NATunnels), f(p.NACPU), f(p.NANetRx), f(p.NANetTx)}
}

// exportData serves GET /api/export?what=…&from=YYYY-MM-DD&to=YYYY-MM-DD&format=csv|json
// as a file download.
func exportData(w http.ResponseWriter, r *http.Request, st *store, up *upstreams) {
	q := r.URL.Query()
	what, format := q.Get("what"), q.Get("format")
	if format != "json" {
		format = "csv"
	}
	from, err1 := time.ParseInLocation("2006-01-02", q.Get("from"), st.loc)
	to, err2 := time.ParseInLocation("2006-01-02", q.Get("to"), st.loc)
	if err1 != nil || err2 != nil || to.Before(from) {
		http.Error(w, "from and to must be dates (YYYY-MM-DD), from ≤ to", http.StatusBadRequest)
		return
	}
	to = to.Add(24*time.Hour - time.Millisecond)

	var header []string
	var rows [][]string
	var jsonOut any

	switch what {
	case "metrics", "minutes", "hourly":
		var pts []sample
		switch what {
		case "metrics":
			if to.Sub(from) > 31*24*time.Hour {
				http.Error(w, "5-second data: export at most 31 days at a time", http.StatusBadRequest)
				return
			}
			pts = st.raw5s(from, to)
		case "minutes":
			pts = st.minutes(from, to)
		default:
			pts = st.hourly(from, to)
		}
		header, jsonOut = sampleCols, pts
		for _, p := range pts {
			rows = append(rows, sampleRow(p, st.loc))
		}

	case "sessions":
		recs := st.records("sessions.jsonl", from, to)
		header = []string{"start", "end", "duration_s", "address", "name", "port", "id", "title", "mode", "version", "listed", "bedrock",
			"unique_players", "peak_players", "peak_bedrock", "connections", "bytes_in", "bytes_out", "region"}
		jsonOut = recs
		for _, m := range recs {
			row := []string{msTime(m["start"], st.loc), msTime(m["end"], st.loc)}
			for _, k := range header[2:] {
				row = append(row, cell(m[k]))
			}
			rows = append(rows, row)
		}

	case "events":
		recs := st.records("events.jsonl", from, to)
		header = []string{"time", "type", "details"}
		jsonOut = recs
		for _, m := range recs {
			details := map[string]any{}
			for k, v := range m {
				if k != "t" && k != "type" {
					details[k] = v
				}
			}
			b, _ := json.Marshal(details)
			rows = append(rows, []string{msTime(m["t"], st.loc), cell(m["type"]), string(b)})
		}

	case "downloads":
		recs := st.records("downloads.jsonl", from, to)
		header = []string{"day", "release", "file", "downloads_total_so_far"}
		jsonOut = recs
		for _, m := range recs {
			rels, _ := m["github_releases"].([]any)
			for _, rel := range rels {
				rm, _ := rel.(map[string]any)
				assets, _ := rm["assets"].([]any)
				for _, a := range assets {
					am, _ := a.(map[string]any)
					rows = append(rows, []string{cell(m["day"]), cell(rm["tag"]), cell(am["name"]), cell(am["count"])})
				}
			}
			if sd, ok := m["store_days"].(map[string]any); ok {
				for day, n := range sd {
					rows = append(rows, []string{day, "microsoft-store", "installs that day", cell(n)})
				}
			}
		}

	case "website":
		web, err := up.get(up.web, "/stats")
		if err != nil {
			http.Error(w, "website stats unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		days, _ := web["days"].(map[string]any)
		keys := make([]string, 0, len(days))
		for d := range days {
			if d >= from.Format("2006-01-02") && d <= to.Format("2006-01-02") {
				keys = append(keys, d)
			}
		}
		sort.Strings(keys)
		header = []string{"day", "kind", "key", "count"}
		filtered := map[string]any{}
		for _, d := range keys {
			dm, _ := days[d].(map[string]any)
			filtered[d] = dm
			rows = append(rows, []string{d, "total", "page_loads", cell(dm["views"])}, []string{d, "total", "visitors", cell(dm["visitors"])})
			for _, kind := range []string{"paths", "referrers", "countries", "crawlers"} {
				m, _ := dm[kind].(map[string]any)
				ks := make([]string, 0, len(m))
				for k := range m {
					ks = append(ks, k)
				}
				sort.Strings(ks)
				for _, k := range ks {
					rows = append(rows, []string{d, kind, k, cell(m[k])})
				}
			}
		}
		jsonOut = filtered

	default:
		http.Error(w, "unknown dataset", http.StatusBadRequest)
		return
	}

	name := fmt.Sprintf("voxelport-%s-%s-to-%s.%s", what, from.Format("2006-01-02"), to.Format("2006-01-02"), format)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", " ")
		enc.Encode(jsonOut)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	cw := csv.NewWriter(w)
	cw.Write(header)
	cw.WriteAll(rows)
}

func msTime(v any, loc *time.Location) string {
	f, ok := v.(float64)
	if !ok {
		return ""
	}
	return time.UnixMilli(int64(f)).In(loc).Format(time.RFC3339)
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		// Server titles come from strangers: stop spreadsheets reading them as formulas.
		if x != "" && strings.ContainsRune("=+-@	", rune(x[0])) {
			return "'" + x
		}
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
