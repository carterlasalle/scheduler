package dashboard

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

//go:embed static/htmx.min.js
var staticFS embed.FS

//go:embed templates/*.html
var templatesFS embed.FS

// htmxJS is the bundled htmx library, loaded via Go embed so the dashboard
// works offline (no CDN dependency at runtime).
var htmxJS = mustReadStatic("static/htmx.min.js")

// Generator produces the fleet dashboard as a single-file HTML page.
type Generator struct {
	db                *sql.DB
	tmpl              *template.Template // parsed once, reused
	fleetTmpl         *template.Template // partial: project table body only
	projectTmpl       *template.Template // full page: /projects/{name}
	queueTmpl         *template.Template // full page: /queue
	tickHistoryTmpl   *template.Template // full page: /ticks
	namespaceViewTmpl *template.Template // full page: /namespaces/{id}
	healthTmpl        *template.Template // full page: /health
	gatewayURL        string
	duckbrainURL      string // optional; health panel probes its /health
	healthClient      *http.Client
	started           time.Time
}

// NewGenerator creates a dashboard generator. Template is parsed at construction
// time so hot-path Generate() never pays the parse cost. gatewayURL is optional;
// when supplied, the health panel probes its /health endpoint.
func NewGenerator(db *sql.DB, gatewayURL ...string) *Generator {
	tmpl := loadTemplates()
	var gateway string
	if len(gatewayURL) > 0 {
		gateway = strings.TrimRight(gatewayURL[0], "/")
	}
	g := &Generator{
		db:                db,
		tmpl:              tmpl,
		fleetTmpl:         tmpl.Lookup("fleet_table"),
		projectTmpl:       tmpl.Lookup("project_detail"),
		queueTmpl:         tmpl.Lookup("queue"),
		tickHistoryTmpl:   tmpl.Lookup("tick_history"),
		namespaceViewTmpl: tmpl.Lookup("namespace_view"),
		healthTmpl:        tmpl.Lookup("health"),
		gatewayURL:        gateway,
		healthClient:      &http.Client{Timeout: 2 * time.Second},
		started:           time.Now(),
	}
	for name, parsed := range map[string]*template.Template{
		"fleet_table":    g.fleetTmpl,
		"project_detail": g.projectTmpl,
		"queue":          g.queueTmpl,
		"tick_history":   g.tickHistoryTmpl,
		"namespace_view": g.namespaceViewTmpl,
		"health":         g.healthTmpl,
	} {
		if parsed == nil {
			panic("dashboard: " + name + " template not registered")
		}
	}
	return g
}

// SetDuckBrainURL registers the DuckBrain HTTP endpoint so the health
// panel can probe it (mirrors gateway probing). Optional.
func (g *Generator) SetDuckBrainURL(u string) {
	g.duckbrainURL = strings.TrimRight(u, "/")
}

// HTMXJS returns the bundled htmx library bytes for serving via HTTP.
func (g *Generator) HTMXJS() []byte { return htmxJS }

// Generate writes the dashboard HTML to w. Template is pre-parsed — zero hot-path overhead.
func (g *Generator) Generate(w io.Writer) error {
	ctx := context.Background()
	data := g.collect(ctx)
	return g.tmpl.ExecuteTemplate(w, "page", data)
}

// GenerateFleetTable renders the fleet table partial (tbody only) for htmx
// to swap into the dashboard page. Routes get this from /dashboard/partial.
func (g *Generator) GenerateFleetTable(w io.Writer) error {
	ctx := context.Background()
	data := g.collect(ctx)
	return g.fleetTmpl.Execute(w, data)
}

// GenerateProjectDetail renders the project detail page. Returns an error
// wrapping ErrProjectNotFound when no project matches the given name.
func (g *Generator) GenerateProjectDetail(w io.Writer, name string) error {
	if name == "" {
		return errors.New("project name is required")
	}
	ctx := context.Background()
	project, err := database.GetProject(ctx, g.db, name)
	if err != nil {
		return fmt.Errorf("load project %q: %w", name, err)
	}

	data := ProjectDetailData{Project: project}

	// Board progress + next-tick timing from the project workdir/cooldown.
	if project.Workdir != "" {
		data.BoardDone, data.BoardTotal = readBoardProgress(filepath.Join(project.Workdir, ".coding-hermes", "tasks.md"))
	}
	running := false
	var lastCompleted string
	_ = g.db.QueryRowContext(ctx, `SELECT COALESCE(last_tick_completed, '') FROM projects WHERE name = ?`, name).Scan(&lastCompleted)
	if latest, err := latestTickForProject(ctx, g.db, name); err == nil {
		data.LatestTick = latest
		running = latest != nil && latest.Status == database.StatusRunning
	}
	data.NextTickIn = nextTickIn(running, lastCompleted, project.CooldownS)

	// Last 20 ticks for the history table.
	if ticks, err := database.ListTicks(ctx, g.db, name, 20); err == nil {
		data.RecentTicks = ticks
	}

	return g.projectTmpl.Execute(w, data)
}

const tickHistoryPageSize = 50

// GenerateTickHistory renders one page of the global tick history. Pages are
// one-based; values below one are normalized to the first page.
func (g *Generator) GenerateTickHistory(w io.Writer, page int) error {
	ctx := context.Background()
	if page < 1 {
		page = 1
	}

	var total int
	if err := g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks`).Scan(&total); err != nil {
		return fmt.Errorf("count ticks: %w", err)
	}
	totalPages := (total + tickHistoryPageSize - 1) / tickHistoryPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	ticks, err := database.ListAllTicks(ctx, g.db, tickHistoryPageSize, (page-1)*tickHistoryPageSize)
	if err != nil {
		return fmt.Errorf("load tick history page %d: %w", page, err)
	}
	data := TickHistoryData{
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Ticks:        ticks,
		Page:         page,
		PageSize:     tickHistoryPageSize,
		TotalTicks:   total,
		TotalPages:   totalPages,
		HasPrevious:  page > 1,
		PreviousPage: page - 1,
		HasNext:      page < totalPages,
		NextPage:     page + 1,
	}
	return g.tickHistoryTmpl.Execute(w, data)
}

// GenerateNamespaceView renders namespace configuration, assigned projects,
// and recent utilization history.
func (g *Generator) GenerateNamespaceView(w io.Writer, id string) error {
	if id == "" {
		return errors.New("namespace id is required")
	}
	ctx := context.Background()
	namespace, err := database.GetNamespace(ctx, g.db, id)
	if err != nil {
		return fmt.Errorf("load namespace %q: %w", id, err)
	}
	projects, err := database.ListProjectsByNamespace(ctx, g.db, id)
	if err != nil {
		return fmt.Errorf("load projects for namespace %q: %w", id, err)
	}
	ticks, err := database.ListNamespaceTicks(ctx, g.db, id, 50)
	if err != nil {
		return fmt.Errorf("load utilization for namespace %q: %w", id, err)
	}

	data := NamespaceViewData{
		Namespace:   namespace,
		Projects:    projects,
		RecentTicks: ticks,
	}
	for _, project := range projects {
		if project.Enabled {
			data.EnabledProjects++
			data.TotalWeight += project.Weight
		}
	}
	if len(ticks) > 0 {
		data.LatestTick = &ticks[0]
		if ticks[0].Allocated > 0 {
			data.Utilization = float64(ticks[0].Used) / float64(ticks[0].Allocated) * 100
		}
	}
	return g.namespaceViewTmpl.Execute(w, data)
}

// GenerateHealth renders daemon, database, and gateway liveness information.
// The page refreshes itself with htmx, so every render performs fresh probes.
func (g *Generator) GenerateHealth(w io.Writer) error {
	ctx := context.Background()
	data := HealthData{
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
		DaemonStatus:   "running",
		DatabaseStatus: "connected",
		GatewayStatus:  "not configured",
		GatewayURL:     g.gatewayURL,
		Uptime:         time.Since(g.started).Round(time.Second).String(),
		Goroutines:     runtime.NumGoroutine(),
	}
	if err := g.db.PingContext(ctx); err != nil {
		data.DatabaseStatus = "error"
	}
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks WHERE status = 'running'`).Scan(&data.ActiveTicks)
	_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticks`).Scan(&data.TotalTicks)

	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	data.MemoryMB = float64(memory.Alloc) / (1024 * 1024)

	if g.gatewayURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.gatewayURL+"/health", nil)
		if err != nil {
			data.GatewayStatus = "error"
		} else {
			resp, err := g.healthClient.Do(req)
			if err != nil {
				data.GatewayStatus = "unreachable"
			} else {
				if resp.StatusCode == http.StatusOK {
					data.GatewayStatus = "connected"
				} else {
					data.GatewayStatus = fmt.Sprintf("unhealthy (HTTP %d)", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
		}
	}

	// DuckBrain probe (fallback visibility): show reachable/unreachable and
	// any spooled writes pending replay. The sync layer spools failed writes,
	// so "unreachable" here is not data loss — it's queued for replay.
	data.DuckBrainStatus = "not configured"
	data.DuckBrainBaseURL = g.duckbrainURL
	if g.duckbrainURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.duckbrainURL+"/health", nil)
		if err != nil {
			data.DuckBrainStatus = "error"
		} else {
			resp, err := g.healthClient.Do(req)
			if err != nil {
				data.DuckBrainStatus = "unreachable"
			} else {
				if resp.StatusCode == http.StatusOK {
					data.DuckBrainStatus = "connected"
				} else {
					data.DuckBrainStatus = fmt.Sprintf("unhealthy (HTTP %d)", resp.StatusCode)
				}
				_ = resp.Body.Close()
			}
		}
		_ = g.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_spool`).Scan(&data.DuckBrainSpooled)
	}
	return g.healthTmpl.Execute(w, data)
}

// GenerateQueue renders the evaluation queue page — all enabled projects
// sorted by urgency (descending) with their weight, priority, and cooldown.
func (g *Generator) GenerateQueue(w io.Writer) error {
	ctx := context.Background()
	data := QueueData{}

	rows, err := g.db.QueryContext(ctx, `
		SELECT p.name, p.weight, p.priority, p.cooldown_s, p.enabled
		FROM projects p
		WHERE p.enabled = 1
		ORDER BY p.name
	`)
	if err != nil {
		return fmt.Errorf("query queue: %w", err)
	}

	// Collect all projects first (close rows before nested queries to avoid
	// SQLite lock contention with modernc.org/sqlite).
	type raw struct {
		name      string
		weight    int
		priority  int
		cooldownS int
		enabled   bool
	}
	var raws []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.name, &r.weight, &r.priority, &r.cooldownS, &r.enabled); err != nil {
			continue
		}
		raws = append(raws, r)
	}
	_ = rows.Close()

	latestTickRows, err := g.db.QueryContext(ctx, `
		SELECT project_name, COALESCE(MAX(spawned_at), '')
		FROM ticks
		WHERE project_name IN (SELECT name FROM projects WHERE enabled = 1)
		GROUP BY project_name
	`)
	if err != nil {
		return fmt.Errorf("query latest queue ticks: %w", err)
	}
	lastTicks := make(map[string]string, len(raws))
	for latestTickRows.Next() {
		var projectName, spawnedAt string
		if err := latestTickRows.Scan(&projectName, &spawnedAt); err != nil {
			_ = latestTickRows.Close()
			return fmt.Errorf("scan latest queue tick: %w", err)
		}
		lastTicks[projectName] = spawnedAt
	}
	if err := latestTickRows.Err(); err != nil {
		_ = latestTickRows.Close()
		return fmt.Errorf("iterate latest queue ticks: %w", err)
	}
	_ = latestTickRows.Close()

	for _, r := range raws {
		e := QueueEntry{
			Name:      r.name,
			Weight:    r.weight,
			Priority:  r.priority,
			CooldownS: r.cooldownS,
			Enabled:   r.enabled,
			Urgency:   float64(r.priority) * 10.0, // base urgency from priority alone
		}
		if lastTick := lastTicks[r.name]; lastTick != "" {
			if t, err := time.Parse(time.RFC3339, lastTick); err == nil {
				e.Urgency = float64(r.priority) * (1 + time.Since(t).Hours())
			}
		}
		data.Entries = append(data.Entries, e)
		data.TotalWeight += r.weight
	}

	// Sort by urgency descending.
	sort.Slice(data.Entries, func(i, j int) bool {
		return data.Entries[i].Urgency > data.Entries[j].Urgency
	})

	data.Count = len(data.Entries)
	return g.queueTmpl.Execute(w, data)
}

const pageTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>Coding Hermes Fleet</title>
<script src="/static/htmx.min.js"></script>
<style>
:root{
--bg:#0d1117;--fg:#c9d1d9;--accent:#58a6ff;--green:#3fb950;--red:#f85149;--yellow:#d2991d;--muted:#8b949e;--border:#21262d;--card:#161b22;--card2:#1c2128;
--ease-out:cubic-bezier(0.23,1,0.32,1);--ease-in-out:cubic-bezier(0.77,0,0.175,1);
}
*{box-sizing:border-box;margin:0;padding:0}
html{-webkit-text-size-adjust:100%}
body{font-family:system-ui,-apple-system,'Segoe UI',sans-serif;font-optical-sizing:auto;background:var(--bg);color:var(--fg);padding:0;margin:0}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}
.wrap{max-width:1240px;margin:0 auto;padding:16px}
h1{font-size:1.4rem;letter-spacing:-0.02em;line-height:1.05;margin-bottom:2px}
h2{font-size:1.05rem;letter-spacing:-0.01em;margin:26px 0 10px}
.meta{color:var(--muted);font-size:0.8rem}
header.sticky{position:sticky;top:0;z-index:10;background:rgba(13,17,23,0.72);backdrop-filter:blur(18px) saturate(160%);-webkit-backdrop-filter:blur(18px) saturate(160%);border-bottom:1px solid var(--border);box-shadow:inset 0 1px 0 rgba(255,255,255,0.04)}
header .inner{max-width:1240px;margin:0 auto;padding:12px 16px;display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:8px}
.nav{display:flex;gap:4px}
.nav a{color:var(--muted);font-size:0.85rem;padding:6px 12px;border-radius:8px;transition:color 160ms var(--ease-out),background 160ms var(--ease-out)}
.nav a:hover{color:var(--fg);background:var(--card2);text-decoration:none}
.nav a.active{color:var(--accent);background:rgba(88,166,255,0.12)}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:20px}
.card{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:14px;opacity:0;transform:translateY(10px);transition:opacity 340ms var(--ease-out),transform 340ms var(--ease-out)}
body.ready .card{opacity:1;transform:translateY(0)}
.card:nth-child(2){transition-delay:40ms}.card:nth-child(3){transition-delay:80ms}.card:nth-child(4){transition-delay:120ms}.card:nth-child(5){transition-delay:160ms}.card:nth-child(6){transition-delay:200ms}
.card .label{color:var(--muted);font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em}
.card .value{font-size:1.5rem;font-weight:600;margin-top:4px;font-variant-numeric:tabular-nums}
.budget-bar{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:14px;margin-bottom:16px}
.budget-fill{height:8px;background:linear-gradient(90deg,var(--green),var(--yellow),var(--red));border-radius:4px;margin-top:6px;transition:width .3s var(--ease-out)}
.budget-label{display:flex;justify-content:space-between;font-size:0.8rem;margin-top:6px;color:var(--muted);font-variant-numeric:tabular-nums}
.prog{background:var(--border);border-radius:4px;height:6px;width:88px;margin-bottom:3px;overflow:hidden}
.prog-fill{height:6px;background:var(--accent);border-radius:4px;transition:width .3s var(--ease-out)}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:10px;overflow:hidden;font-size:0.85rem}
th,td{padding:9px 12px;text-align:left;border-bottom:1px solid var(--border);vertical-align:middle}
th{background:var(--card2);color:var(--muted);font-weight:600;text-transform:uppercase;font-size:0.68rem;letter-spacing:0.05em;position:sticky;top:0}
tr:last-child td{border-bottom:none}
tbody tr{transition:background 160ms var(--ease-out)}
@media(hover:hover) and (pointer:fine){tbody tr:hover{background:var(--card2)}}
.status-ok{color:var(--green)}.status-fail{color:var(--red)}.status-running{color:var(--accent)}
.status-timeout{color:var(--yellow)}
.running-dot{display:inline-block;width:6px;height:6px;background:var(--accent);border-radius:50%;margin-right:5px;animation:pulse 2.4s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.35}}
.flash{animation:flashbg 220ms var(--ease-out)}
@keyframes flashbg{0%{background:rgba(88,166,255,0.18)}100%{background:transparent}}
.fail-flag{color:var(--red);font-weight:600}
.util-green{color:var(--green)}.util-yellow{color:var(--yellow)}.util-red{color:var(--red)}
.utilization-bar{display:inline-block;height:6px;background:var(--accent);border-radius:3px;margin-right:4px;vertical-align:middle;max-width:60px}
.disabled{opacity:0.45}
.spark{display:block;margin-top:2px}
.num{font-variant-numeric:tabular-nums}
.htmx-indicator{color:var(--muted);font-size:0.7rem;margin-left:8px;display:none}
.htmx-request .htmx-indicator{display:inline}
@media(max-width:640px){table{font-size:0.72rem}th,td{padding:6px 8px}header .inner{flex-direction:column;align-items:flex-start}}
@media (prefers-reduced-motion: reduce){
  .card{opacity:1;transform:none;transition:none}
  .running-dot{animation:none}
}
@media (prefers-reduced-transparency: reduce){
  header.sticky{background:var(--bg);backdrop-filter:none;-webkit-backdrop-filter:none}
}
@media (prefers-contrast: more){
  body{background:#000}.card,table,.budget-bar{background:#111;border-color:#444}
}
</style>
</head>
<body>
<div id="app">
<header class="sticky"><div class="inner">
<a href="/" style="font-weight:700;color:var(--fg);letter-spacing:-0.01em">🚀 Coding Hermes Fleet</a>
<div class="nav">
<a href="/" class="active">Overview</a>
<a href="/queue">Queue</a>
<a href="/ticks">Ticks</a>
<a href="/health">Health</a>
</div>
</div></header>
<div class="wrap">
<div class="meta">Generated {{.GeneratedAt}} · auto-refresh 60s · live via htmx 10s</div>

<div class="cards">
<div class="card"><div class="label">Enabled Projects</div><div class="value">{{.EnabledProjects}}/{{.TotalProjects}}</div></div>
<div class="card"><div class="label">Active Ticks</div><div class="value">{{.ActiveTicks}}</div></div>
<div class="card"><div class="label">Budget Used</div><div class="value">{{.BudgetUsed}}/{{.BudgetTotal}}</div></div>
{{if .CostTodayTotal}}<div class="card"><div class="label">Cost Today</div><div class="value">${{printf "%.2f" .CostTodayTotal}}</div></div>{{end}}
{{if .CostWeekTotal}}<div class="card"><div class="label">Cost 7d</div><div class="value">${{printf "%.2f" .CostWeekTotal}}</div></div>{{end}}
</div>

<div class="budget-bar">
<div class="budget-label"><span>Weight Budget</span><span>{{.BudgetUsed}}/{{.BudgetTotal}}</span></div>
<div class="budget-fill" style="width:{{percent .BudgetUsed .BudgetTotal}}%"></div>
</div>

<h2>Projects</h2>
<table>
<thead><tr><th>Project</th><th>W</th><th>P</th><th>Last Tick</th><th>Outcome</th><th>Progress</th><th>Steps Left</th><th>Next Tick</th><th>Cost</th><th>Recent</th></tr></thead>
<tbody id="fleet-overview"
hx-get="/dashboard/partial"
hx-trigger="every 10s"
hx-swap="innerHTML">
{{range .Projects}}
<tr class="{{if not .Enabled}}disabled{{end}}">
<td><a href="/projects/{{.Name}}" style="color:var(--accent);text-decoration:none">{{.Name}}</a>{{if .RecentFailures}} <span class="fail-flag" title="{{.RecentFailures}} of last {{.RecentTicks}} ticks failed/timed out">●</span>{{end}}</td>
<td class="num">{{.Weight}}</td>
<td class="num">{{.Priority}}</td>
<td class="meta">{{shortTime .LastTick}}</td>
<td class="{{if eq .LastOutcome "committed"}}status-ok{{else if eq .LastOutcome "failed"}}status-fail{{else if eq .LastOutcome "timeout"}}status-timeout{{end}}">{{if .LastOutcome}}{{.LastOutcome}}{{else}}—{{end}}</td>
<td>
{{if .BoardTotal}}
<div class="prog"><div class="prog-fill" style="width:{{percent .BoardDone .BoardTotal}}%"></div></div>
<span class="meta">{{.BoardDone}}/{{.BoardTotal}} · {{percent .BoardDone .BoardTotal}}%</span>
{{else}}
<span class="meta">—</span>
{{end}}
</td>
<td>{{if .BoardTotal}}<span class="meta num">{{sub .BoardTotal .BoardDone}} left</span>{{else}}<span class="meta">—</span>{{end}}</td>
<td class="{{if eq .NextTickIn "running"}}status-running{{else if eq .NextTickIn "due now"}}status-fail{{end}}">{{if .NextTickIn}}{{.NextTickIn}}{{else}}—{{end}}</td>
<td class="num">{{if .CostToday}}<span title="today">${{printf "%.3f" .CostToday}}</span>{{else}}<span class="meta">—</span>{{end}}{{if sparkline .CostSeries}}<br>{{sparkline .CostSeries}}{{end}}</td>
<td class="num">{{if .RecentFailures}}<span class="status-fail">{{.RecentFailures}}/{{.RecentTicks}}</span>{{else if .RecentTicks}}<span class="status-ok">{{.RecentTicks}} ok</span>{{else}}<span class="meta">—</span>{{end}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>Recent Ticks</h2>
<table>
<thead><tr><th>Project</th><th>Status</th><th>Outcome</th><th>Duration</th><th>Spawned</th><th>Commits</th><th>Files</th></tr></thead>
<tbody>
{{range .RecentTicks}}
<tr>
<td>{{.Project}}</td>
<td class="{{if eq .Status "completed"}}status-ok{{else if eq .Status "failed"}}status-fail{{else if eq .Status "timeout"}}status-timeout{{else if eq .Status "running"}}status-running{{end}}">{{.Status}}</td>
<td>{{if .Outcome}}{{.Outcome}}{{else}}—{{end}}</td>
<td class="num">{{if .Duration}}{{.Duration}}{{else}}<span class="meta">—</span>{{end}}</td>
<td class="meta">{{shortTime .SpawnedAt}}</td>
<td class="num">{{.Commits}}</td>
<td class="num">{{.FilesChanged}}</td>
</tr>{{end}}
</tbody>
</table>

<h2>Namespaces</h2>
{{if .Namespaces}}
<table>
<thead><tr><th>Namespace</th><th>Weight</th><th>Reserved</th><th>Hard Cap</th><th>Allocated</th><th>Used</th><th>Utilization</th><th>Borrowed</th><th>Lent</th><th>Projects</th></tr></thead>
<tbody>
{{range .Namespaces}}
<tr class="{{utilClass .Reserved .HardCap .Used}}">
  <td>{{.ID}}</td>
  <td>{{.Weight}}</td>
  <td>{{.Reserved}}</td>
  <td>{{if .HardCap}}{{.HardCap}}{{else}}∞{{end}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td><div class="utilization-bar" style="width:{{printf "%.0f" .Utilization}}%;background:{{utilColor .Utilization}}"></div>{{printf "%.0f" .Utilization}}%</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td>{{.ProjectCount}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}
<p class="meta">No namespaces configured</p>
{{end}}

<h2>Namespace Utilization History</h2>
{{if .NamespaceTicks}}
<table>
<thead><tr><th>Namespace</th><th>Tick Group</th><th>Allocated</th><th>Used</th><th>Borrowed</th><th>Lent</th><th>Time</th></tr></thead>
<tbody>
{{range .NamespaceTicks}}
<tr>
  <td>{{.NamespaceID}}</td>
  <td>{{.TickGroup}}</td>
  <td>{{.Allocated}}</td>
  <td>{{.Used}}</td>
  <td>{{if .Borrowed}}+{{.Borrowed}}{{end}}</td>
  <td>{{if .Lent}}-{{.Lent}}{{end}}</td>
  <td class="meta">{{shortTime .CreatedAt}}</td>
</tr>{{end}}
</tbody>
</table>
{{else}}
<p class="meta">No namespace tick data available</p>
{{end}}
</div><!-- /wrap -->
</div><!-- /app -->
<script>
// Entrance stagger: add .ready after double-rAF so the stat cards fade/slide
// in once on first load. Fallback for @starting-style. Never re-runs on the
// 10s htmx poll (only body.ready once).
(function () {
  var reduced = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (reduced) { document.body.classList.add('ready'); return; }
  requestAnimationFrame(function () {
    requestAnimationFrame(function () { document.body.classList.add('ready'); });
  });
})();
</script>
</body>
</html>`
