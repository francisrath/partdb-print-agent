package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

// weeksShown is how many weeks the installs chart covers.
const weeksShown = 26

type row struct {
	Name  string
	N     int
	Share int // percent of the largest row, for the bar
}

type page struct {
	Reports, Weeks                                                          int
	Week                                                                    string // the latest week with reports
	Installs                                                                int    // reports that week
	PerWeek                                                                 []row
	Versions, OS, Install, Boards, Models, Transports, Jobs, Labels, Errors []row
	TotalLabels                                                             int
}

// summary reads every stored report: installs per week, and for the latest week what they run.
// Labels, jobs and errors are summed over all weeks.
// ponytail: reads the whole file per request; cache it when the file gets large.
func summary(path string) (*page, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return &page{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var reps []stored
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var s stored
		if json.Unmarshal(sc.Bytes(), &s) == nil {
			reps = append(reps, s)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	p := &page{Reports: len(reps)}
	perWeek := map[string]int{}
	for _, r := range reps {
		perWeek[r.Week]++
	}
	weeks := make([]string, 0, len(perWeek))
	for w := range perWeek {
		weeks = append(weeks, w)
	}
	slices.Sort(weeks)
	p.Weeks = len(weeks)
	if len(weeks) == 0 {
		return p, nil
	}
	p.Week = weeks[len(weeks)-1]
	p.Installs = perWeek[p.Week]
	shown := weeks[max(0, len(weeks)-weeksShown):]
	pw := map[string]int{}
	for _, w := range shown {
		pw[w] = perWeek[w]
	}
	p.PerWeek = rows(pw, false)
	ver, osv, inst, board, models, trans, jobs, labels, errs := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{},
		map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, r := range reps {
		for k, n := range r.Jobs {
			jobs[k] += n
		}
		for k, n := range r.Labels {
			labels[k] += n
			p.TotalLabels += n
		}
		for k, n := range r.Errors {
			errs[k] += n
		}
		if r.Week != p.Week {
			continue
		}
		ver[r.Version]++
		osv[cmp.Or(r.OSVersion, r.OS)+" ("+r.Arch+")"]++
		inst[r.Install]++
		if r.Board != "" {
			board[r.Board]++
		}
		for k, n := range r.Models {
			models[k] += n
		}
		for k, n := range r.Transports {
			trans[k] += n
		}
	}
	p.Versions, p.OS, p.Install, p.Boards = rows(ver, true), rows(osv, true), rows(inst, true), rows(board, true)
	p.Models, p.Transports = rows(models, true), rows(trans, true)
	p.Jobs, p.Labels, p.Errors = rows(jobs, true), rows(labels, true), rows(errs, true)
	return p, nil
}

// rows sorts by count (byCount) or by name, with bar widths relative to the largest.
func rows(m map[string]int, byCount bool) []row {
	out := make([]row, 0, len(m))
	top := 0
	for k, n := range m {
		out = append(out, row{Name: k, N: n})
		top = max(top, n)
	}
	slices.SortFunc(out, func(a, b row) int {
		if byCount && a.N != b.N {
			return b.N - a.N
		}
		return strings.Compare(a.Name, b.Name)
	})
	for i := range out {
		if top > 0 {
			out[i].Share = out[i].N * 100 / top
		}
	}
	return out
}

func (c *collector) stats(w http.ResponseWriter, _ *http.Request) {
	p, err := summary(c.path)
	if err != nil {
		http.Error(w, "can't read the reports", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = statsPage.Execute(w, p)
}

var statsPage = template.Must(template.New("stats").Funcs(template.FuncMap{
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>pdb-print usage</title>
<style>
:root { --fg: #1d2a30; --muted: #5d7480; --bar: #2f7d78; --bg: #fff; --line: #e3ebee; }
@media (prefers-color-scheme: dark) { :root { --fg: #e3ebee; --muted: #9db4bf; --bar: #4fb3ac; --bg: #11181b; --line: #24323a; } }
body { font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; color: var(--fg);
  background: var(--bg); max-width: 960px; margin: 0 auto; padding: 24px 16px; }
h1 { margin: 0 0 4px; font-size: 26px; } h2 { font-size: 16px; margin: 0 0 8px; }
.muted { color: var(--muted); } a { color: var(--bar); }
.big { display: flex; gap: 32px; flex-wrap: wrap; margin: 20px 0 28px; }
.big div { font-size: 13px; color: var(--muted); } .big b { display: block; font-size: 28px; color: var(--fg); }
.grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 28px 32px; }
table { width: 100%; border-collapse: collapse; } td { padding: 3px 0; border-bottom: 1px solid var(--line); font-size: 14px; }
td.n { text-align: right; width: 4em; font-variant-numeric: tabular-nums; }
td.bar { width: 40%; padding-left: 10px; } td.bar span { display: block; height: 8px; border-radius: 4px; background: var(--bar); }
.weeks { display: flex; align-items: flex-end; gap: 3px; height: 120px; border-bottom: 1px solid var(--line); }
.weeks div { flex: 1; background: var(--bar); border-radius: 3px 3px 0 0; min-height: 2px; }
.weeklabels { display: flex; justify-content: space-between; font-size: 12px; color: var(--muted); margin-top: 4px; }
footer { margin-top: 36px; font-size: 13px; }
</style></head><body>
<h1>pdb-print usage</h1>
<p class="muted">From the anonymous weekly reports pdb-print installs send
(<a href="https://github.com/francisrath/partdb-print-agent/blob/main/docs/telemetry.md">what they contain</a>).
Installs are reports per week: there is no install id.</p>
{{if not .Reports}}<p>No reports yet.</p>{{else}}
<div class="big">
  <div><b>{{.Installs}}</b>installs in {{.Week}}</div>
  <div><b>{{.TotalLabels}}</b>labels printed, all weeks</div>
  <div><b>{{.Weeks}}</b>weeks of reports</div>
</div>
<h2>Installs per week</h2>
<div class="weeks">{{range .PerWeek}}<div style="height: {{.Share}}%" title="{{.Name}}: {{.N}}"></div>{{end}}</div>
<div class="weeklabels">{{with index .PerWeek 0}}<span>{{.Name}}</span>{{end}}<span>{{.Week}}</span></div>
<br>
<div class="grid">
{{template "t" (dict "T" (print "Version, " .Week) "R" .Versions)}}
{{template "t" (dict "T" (print "Operating system, " .Week) "R" .OS)}}
{{template "t" (dict "T" (print "Installed with, " .Week) "R" .Install)}}
{{template "t" (dict "T" (print "Boards, " .Week) "R" .Boards)}}
{{template "t" (dict "T" (print "Printer models, " .Week) "R" .Models)}}
{{template "t" (dict "T" (print "Connections, " .Week) "R" .Transports)}}
{{template "t" (dict "T" "Labels per model, all weeks" "R" .Labels)}}
{{template "t" (dict "T" "Jobs per source, all weeks" "R" .Jobs)}}
{{template "t" (dict "T" "Errors, all weeks" "R" .Errors)}}
</div>{{end}}
<footer class="muted">Collected by <a href="https://github.com/francisrath/partdb-print-agent/tree/main/cmd/pdb-print-stats">pdb-print-stats</a>,
which stores no addresses. Turn reports off with <code>telemetry: false</code>.</footer>
</body></html>
{{define "t"}}<section><h2>{{.T}}</h2>{{if .R}}<table>{{range .R}}<tr><td>{{.Name}}</td><td class="n">{{.N}}</td><td class="bar"><span style="width: {{.Share}}%"></span></td></tr>{{end}}</table>{{else}}<p class="muted">none</p>{{end}}</section>{{end}}`))

// badge is a shields.io endpoint (https://shields.io/badges/endpoint-badge): the installs that reported
// in the latest week. README: ![installs](https://img.shields.io/endpoint?url=https://pdb-print-stats.cloudline.no/v1/badge/installs.json)
func (c *collector) badge(w http.ResponseWriter, _ *http.Request) {
	p, err := summary(c.path)
	if err != nil {
		http.Error(w, "can't read the reports", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"schemaVersion": 1, "label": "active installs", "message": strconv.Itoa(p.Installs), "color": "2f7d78",
	})
}
