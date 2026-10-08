package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type PhaseRow struct {
	Name  string           `json:"name"`
	Stats Stats            `json:"stats"`
	RSSKB int64            `json:"rss_kb,omitempty"`
	Files map[string]int64 `json:"files,omitempty"`
	Note  string           `json:"note,omitempty"`
}

type Case struct {
	Scenario  string           `json:"scenario"`
	Variant   string           `json:"variant"`
	Params    map[string]any   `json:"params"`
	Seconds   float64          `json:"seconds"`
	Total     Stats            `json:"total"`
	Ops       map[string]Stats `json:"ops,omitempty"`
	Phases    []PhaseRow       `json:"phases,omitempty"`
	RSSBefore int64            `json:"rss_before_kb"`
	RSSAfter  int64            `json:"rss_after_kb"`
	RSSPeak   int64            `json:"rss_peak_kb"`
	CPUPct    float64          `json:"server_cpu_pct_of_one_core"`
	FilesPre  map[string]int64 `json:"files_before"`
	FilesPost map[string]int64 `json:"files_after"`
	LoadAvg   string           `json:"loadavg_start"`
	Extra     map[string]any   `json:"extra,omitempty"`
	Notes     []string         `json:"notes,omitempty"`
	Verdicts  []string         `json:"flags,omitempty"`
}

type Report struct {
	Tag     string         `json:"tag"`
	Date    string         `json:"date"`
	Host    map[string]any `json:"host"`
	Dataset *Dataset       `json:"dataset"`
	Cases   []*Case        `json:"cases"`
}

func mb(kb int64) string { return fmt.Sprintf("%.0f", float64(kb)/1024) }
func fb(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

func statusStr(m map[string]int64) string {
	var ks []string
	for k, v := range m {
		if !strings.HasPrefix(k, "2") {
			ks = append(ks, fmt.Sprintf("%s:%d", k, v))
		}
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return "0"
	}
	return strings.Join(ks, " ")
}

func paramStr(p map[string]any) string {
	var ks []string
	for k := range p {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var o []string
	for _, k := range ks {
		o = append(o, fmt.Sprintf("%s=%v", k, p[k]))
	}
	return strings.Join(o, " ")
}

func (r *Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# TokiBase load results (%s, %s)\n\n", r.Tag, r.Date)
	b.WriteString("Host: ")
	var ks []string
	for k := range r.Host {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(&b, "%s=`%v` ", k, r.Host[k])
	}
	b.WriteString("\n\n")
	if r.Dataset != nil {
		fmt.Fprintf(&b, "Dataset: %d FGR collections, %d records. Largest base collections used for reads:\n\n", r.Dataset.Collections, r.Dataset.TotalRecs)
		for _, c := range r.Dataset.Top {
			fmt.Fprintf(&b, "- `%s`: %d records, sort by created=%v, expand=`%s`, listRule set to auth-required=%v\n", c.Name, c.Records, c.Created, c.Expand, c.RuleSet)
		}
		b.WriteString("\n")
	}
	scen := []string{}
	seen := map[string]bool{}
	for _, c := range r.Cases {
		if !seen[c.Scenario] {
			seen[c.Scenario] = true
			scen = append(scen, c.Scenario)
		}
	}
	for _, s := range scen {
		fmt.Fprintf(&b, "## %s\n\n", s)
		b.WriteString("| variant | params | req/s | failed | p50 ms | p95 ms | p99 ms | max ms | RSS MB before/after/peak | server CPU % | load1/5/15 at start |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, c := range r.Cases {
			if c.Scenario != s {
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %.1f | %s | %.1f | %.1f | %.1f | %.0f | %s/%s/%s | %.0f | %s |\n", c.Variant, paramStr(c.Params), c.Total.RPS, statusStr(c.Total.Status), c.Total.P50ms, c.Total.P95ms, c.Total.P99ms, c.Total.MaxMs, mb(c.RSSBefore), mb(c.RSSAfter), mb(c.RSSPeak), c.CPUPct, c.LoadAvg)
		}
		b.WriteString("\n")
		for _, c := range r.Cases {
			if c.Scenario != s {
				continue
			}
			if len(c.Ops) > 1 {
				fmt.Fprintf(&b, "Per op, %s %s:\n\n| op | n | req/s | p50 | p95 | p99 | failed |\n|---|---|---|---|---|---|---|\n", c.Variant, paramStr(c.Params))
				var on []string
				for k := range c.Ops {
					on = append(on, k)
				}
				sort.Strings(on)
				for _, k := range on {
					o := c.Ops[k]
					fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f | %.1f | %.1f | %s |\n", k, o.Count, o.RPS, o.P50ms, o.P95ms, o.P99ms, statusStr(o.Status))
				}
				b.WriteString("\n")
			}
			if len(c.Phases) > 0 {
				fmt.Fprintf(&b, "Phases, %s %s:\n\n| phase | n | req/s | p50 | p95 | p99 | max | failed | RSS MB | data.db-wal | aux.db-wal | note |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n", c.Variant, paramStr(c.Params))
				for _, p := range c.Phases {
					fmt.Fprintf(&b, "| %s | %d | %.1f | %.1f | %.1f | %.1f | %.0f | %s | %s | %s | %s | %s |\n", p.Name, p.Stats.Count, p.Stats.RPS, p.Stats.P50ms, p.Stats.P95ms, p.Stats.P99ms, p.Stats.MaxMs, statusStr(p.Stats.Status), mb(p.RSSKB), fb(p.Files["data.db-wal"]), fb(p.Files["auxiliary.db-wal"]), p.Note)
				}
				b.WriteString("\n")
			}
			if len(c.Extra) > 0 {
				var ek []string
				for k := range c.Extra {
					ek = append(ek, k)
				}
				sort.Strings(ek)
				fmt.Fprintf(&b, "Details, %s %s:\n\n", c.Variant, paramStr(c.Params))
				for _, k := range ek {
					fmt.Fprintf(&b, "- %s: `%v`\n", k, c.Extra[k])
				}
				b.WriteString("\n")
			}
			for _, n := range c.Notes {
				fmt.Fprintf(&b, "- note (%s %s): %s\n", c.Variant, paramStr(c.Params), n)
			}
			for _, n := range c.Verdicts {
				fmt.Fprintf(&b, "- **FLAG** (%s %s): %s\n", c.Variant, paramStr(c.Params), n)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "DB files after last case of this scenario:\n\n")
		for i := len(r.Cases) - 1; i >= 0; i-- {
			if r.Cases[i].Scenario == s {
				var fk []string
				for k := range r.Cases[i].FilesPost {
					fk = append(fk, k)
				}
				sort.Strings(fk)
				for _, k := range fk {
					fmt.Fprintf(&b, "- %s: %s\n", k, fb(r.Cases[i].FilesPost[k]))
				}
				break
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func hostInfo(bin string) map[string]any {
	h := map[string]any{"date_utc": time.Now().UTC().Format(time.RFC3339)}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		n := 0
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "model name") && n == 0 {
				h["cpu"] = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
			}
			if strings.HasPrefix(l, "processor") {
				n++
			}
		}
		h["vcpu"] = n
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "MemTotal:") {
				h["mem"] = strings.TrimSpace(strings.TrimPrefix(l, "MemTotal:"))
			}
		}
	}
	if b, err := os.ReadFile("/proc/version"); err == nil {
		f := strings.Fields(string(b))
		if len(f) > 2 {
			h["kernel"] = f[2]
		}
	}
	h["loadavg_start"] = loadavg()
	return h
}
