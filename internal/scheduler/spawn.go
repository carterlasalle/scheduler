package scheduler

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Cost estimation constants for real ticks where session export is unavailable.
// These are conservative estimates based on typical foreman tick usage.
const (
	estTokensIn    = 8000     // estimated input tokens per tick
	estTokensOut   = 2000     // estimated output tokens per tick
	estCostPerIn   = 0.000002 // foreman-model input $/token (set via env)
	estCostPerOut  = 0.000008 // foreman-model output $/token (set via env)
	estCostPerTick = float64(estTokensIn)*estCostPerIn + float64(estTokensOut)*estCostPerOut
)

// estimateTickCost returns estimated token counts and cost for a real tick.
// Real session export (hermes sessions export) is a future task; for now we
// use fixed estimates so cost aggregation works from day one.
func estimateTickCost() (tokensIn, tokensOut int, costUSD float64) {
	return estTokensIn, estTokensOut, estCostPerTick
}

// Spawner launches coding-hermes foreman processes.
type Spawner struct {
	db             *sql.DB
	maxConcurrent  int
	active         map[string]*exec.Cmd // tickID -> running process
	mu             sync.Mutex
	timeout        time.Duration
	model          string
	provider       string
	skills         string
	foremanHome    string         // HERMES_HOME for foreman config
	gateway        *GatewayClient // HTTP API client (nil = use exec.Command)
	noExecFallback bool           // disable exec.Command fallback on gateway failure

	// Prometheus-style spawn counters since last restart.
	spawnCountHTTP int64
	spawnCountExec int64
}

// NewSpawner creates a spawner with the given concurrency limit and defaults.
func NewSpawner(db *sql.DB, maxConcurrent int, timeout ...time.Duration) *Spawner {
	to := 30 * time.Minute
	if len(timeout) > 0 {
		to = timeout[0]
	}
	return &Spawner{
		db:            db,
		maxConcurrent: maxConcurrent,
		active:        make(map[string]*exec.Cmd),
		timeout:       to,
		model:         getEnvOrDefault("SCHEDULER_FOREMAN_MODEL", "your-model-name"),
		provider:      getEnvOrDefault("SCHEDULER_FOREMAN_PROVIDER", "your-provider-name"),
		skills:        getEnvOrDefault("SCHEDULER_FOREMAN_SKILLS", "coding-hermes-foreman"),
		foremanHome:   os.ExpandEnv("$HOME/.hermes/foreman"),
	}
}

// getEnvOrDefault returns the value of envVar if set, otherwise fallback.
func getEnvOrDefault(envVar, fallback string) string {
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return fallback
}

// noteSpawnFailure increments the project's consecutive spawn-failure counter,
// which drives the exponential selection backoff (S-GAP-001). Best-effort:
// a DB error here must never mask the real spawn error.
func (s *Spawner) noteSpawnFailure(project string) {
	if _, err := s.db.Exec(
		`UPDATE projects SET consecutive_failures = consecutive_failures + 1 WHERE name = ?`,
		project); err != nil {
		log.Printf("WARN: consecutive_failures increment for %s: %v", project, err)
	}
}

// SetForemanHome overrides the default HERMES_HOME for foreman sessions.
func (s *Spawner) SetForemanHome(path string) {
	s.foremanHome = path
}

// RunningSet returns the set of project names that currently have a spawned
// process (in-memory). This is more accurate than the DB query because spawns
// haven't been committed to the DB yet when the packer queries.
func (s *Spawner) RunningSet() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := make(map[string]bool, len(s.active))
	for tickID := range s.active {
		// Extract project name from tick ID: "project-YYYY-MM-DD-HH-MM-SS"
		idx := strings.LastIndex(tickID, "-202")
		if idx > 0 {
			set[tickID[:idx]] = true
		}
	}
	return set
}

// SetGatewayClient configures the HTTP API client. If set, Spawn() prefers
// HTTP over process spawning. Pass nil to disable and fall back to exec.Command.
func (s *Spawner) SetGatewayClient(client *GatewayClient) {
	s.gateway = client
}

// SetNoExecFallback disables the exec.Command fallback when gateway spawns fail.
func (s *Spawner) SetNoExecFallback(v bool) {
	s.noExecFallback = v
}

// GatewayAvailable returns true if the gateway client is configured and reachable.
func (s *Spawner) GatewayAvailable() bool {
	if s.gateway == nil {
		return false
	}
	return s.gateway.Ping(context.Background()) == nil
}

// SpawnMethodCounts returns HTTP and exec spawn counts since last restart.
func (s *Spawner) SpawnMethodCounts() (httpCount, execCount int64) {
	return atomic.LoadInt64(&s.spawnCountHTTP), atomic.LoadInt64(&s.spawnCountExec)
}

// ActiveCount returns the number of currently running spawns.
func (s *Spawner) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// WorkerDefaults returns a prompt suffix with the project's preferred worker
// model and provider. Empty string when neither is configured. Includes
// fallback instructions so the foreman can switch models freely.
func WorkerDefaults(project PackedProject) string {
	if project.WorkerModel == "" && project.WorkerProvider == "" {
		return ""
	}
	m := project.WorkerModel
	p := project.WorkerProvider
	if m == "" {
		m = "(no default)"
	}
	if p == "" {
		p = "(no default)"
	}
	return fmt.Sprintf(
		"Worker model/provider (AUTHORITATIVE, do not change): use model %s with provider %s. "+
			"The board's model column is an advisory routing suggestion only; the configured worker_model here takes precedence and MUST be used for every worker you dispatch. "+
			"Only switch models if this one errors with an actual unavailable/rate-limited failure — do not second-guess based on the board's recommendation. ",
		m, p,
	)
}

// canSpawn checks concurrency limits.
func (s *Spawner) canSpawn() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active) < s.maxConcurrent
}

// Spawn launches a foreman for the given project and tick ID.
// Returns an error only if the process fails to start.
// The spawned process is tracked internally and reaped by the lifecycle tracker.
func (s *Spawner) Spawn(project PackedProject, tickID string) (*SpawnedTick, error) {
	if !s.canSpawn() {
		return nil, fmt.Errorf("max concurrency %d reached", s.maxConcurrent)
	}

	var cmd *exec.Cmd

	if project.Command != "" {
		// Custom command.
		if strings.Contains(project.Command, "bash -c") {
			// Shell one-liner — pass the script string directly to bash -c.
			script := strings.TrimPrefix(project.Command, "bash -c ")
			script = strings.TrimSpace(script)
			// Strip surrounding quotes if present.
			script = strings.Trim(script, "'\"")
			cmd = exec.Command("bash", "-c", script)
		} else {
			parts := splitCommand(project.Command)
			cmd = exec.Command(parts[0], parts[1:]...)
		}
		cmd.Dir = project.Workdir
	} else {
		model := s.model
		if project.Model != "" {
			model = project.Model
		}

		prompt := fmt.Sprintf(
			"[Scheduler tick: %s] "+
				"Load skills coding-hermes-board, coding-hermes-model-router, coding-hermes-never-done, coding-hermes-specs, coding-hermes-testing, coding-hermes-middle-out, systematic-debugging, trust-but-verify, reality-validation, github-pr-workflow, github-repo-management, claude-design, popular-web-designs, hilo, gitreins-usage. "+
				"Read .coding-hermes/tasks.md. Execute ONE foreman tick per the foreman skill. "+
				"Workdir: %s. "+
				"IMPORTANT — worker dispatch: You are the FOREMAN. You pick ONE board task, then dispatch a WORKER to implement it. "+
				"Do NOT implement complex tasks yourself. To dispatch a worker, run a BACKGROUND process via your terminal tool: "+
				"`hermes chat -q \"<task brief from the board, plus files-to-modify and acceptance criteria>\" -m <worker_model> --provider <worker_provider> -s coding-hermes-worker --ignore-rules -Q` "+
				"(terminal background=true). The worker shares this same workdir, so it edits files and commits directly. "+
				"Then poll the background process until it exits, verify build/lint/test and the commit landed, update the board, and report. "+
				"MANDATORY PUSH AFTER EVERY COMMIT — do not skip: after ANY commit (worker or yours), run `git push origin <branch>` (or `git push`) and verify `git fetch origin && git rev-list --count origin/<branch>..HEAD` is 0. A tick that ends with unpushed commits is NOT complete. Never rely on the worker having pushed — verify the remote HEAD yourself. On non-fast-forward push, `git pull --rebase`, re-run the gate, push. "+
				"Only implement trivial one-file changes yourself; anything multi-file or architectural goes to a worker. "+
				"Worker model/provider: %s. "+
				"MANDATORY GitReins lifecycle — do not skip: (1) BEFORE any implementation, run `gitreins task create <TASK-ID> \"<title>\" \"<criterion>\"` then `gitreins task start <TASK-ID>` for the board task you picked. "+
				"(2) AFTER the worker commits the work (verify the commit exists in git log), ALWAYS run `gitreins task complete <TASK-ID>` — this fires the Tier 2 LLM judge and writes verdict.json. "+
				"NEVER end a tick without running `gitreins task complete` for the picked task — even if the tick is near its timeout, complete the gitreins task FIRST, then update the board. "+
				"(3) Then delete the gitreins task with `gitreins task delete <TASK-ID>` to keep tasks.yaml clean. "+
				"If the worker committed but you missed the gitreins lifecycle, run `gitreins task complete` on the committed work before finishing. "+
				"MANDATORY CI-health check — do not skip: run `gh run list --repo <org>/<repo> --limit 3 --json status,conclusion,displayTitle,headBranch,createdAt` (derive org/repo from `git remote -v` — the on-disk folder name may not match the GitHub org). If ANY recent run shows conclusion=failure that YOU did not just create, file a board task for the broken CI (e.g. INT-CI-<n> '<what failed>') before ending the tick, so it does not rot. Report CI health (green or the failure you flagged) in your output. "+
				"Format your final output as clean, well-structured markdown with tables and sections. "+
				"Report result.",
			tickID, project.Workdir,
			WorkerDefaults(project),
		)

		// Try HTTP gateway spawn first (zero process overhead).
		if s.gateway != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
			// Per-foreman gateway key: project.GatewayKey when set, else the
			// daemon's shared --gateway-key (Bane 2026-07-31).
			resp, gwErr := s.gateway.SendResponse(ctx, prompt, model, project.GatewayKey)
			cancel()
			if gwErr == nil && resp != nil {
				atomic.AddInt64(&s.spawnCountHTTP, 1)
				text := resp.ExtractText()
				now := time.Now()
				// NOTE: tick completion is handled by slot_pool → lifecycle.Complete
				// (correct columns + outcome CHECK). The legacy direct UPDATE here was
				// removed in GAP-002 — it referenced non-existent columns
				// (finished_at, output) and outcome='ok' violated the ticks CHECK, so
				// it silently no-oped on every run.
				// S-GAP-001: a successful spawn also resets the consecutive-failure
				// backoff counter.
				_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ?, consecutive_failures = 0 WHERE name = ?`,
					now.Format(time.RFC3339), project.Name)

				log.Printf("GATEWAY: %s tick=%s tokens=%d/%d",
					project.Name, tickID, resp.Usage.InputTokens, resp.Usage.OutputTokens)
				return &SpawnedTick{
					TickID:     tickID,
					Project:    project.Name,
					SessionID:  "gateway",
					Started:    now,
					Deliver:    project.Deliver,
					Output:     *bytes.NewBufferString(text),
					spawner:    s,
					completed:  true,
					completeAt: now,
				}, nil
			}
			log.Printf("GATEWAY FAIL: %s tick=%s error=%v — falling back to exec.Command", project.Name, tickID, gwErr)
			if s.noExecFallback {
				log.Printf("SKIPPED: %s tick=%s exec fallback disabled, dropping tick", project.Name, tickID)
				s.noteSpawnFailure(project.Name)
				return nil, fmt.Errorf("gateway unreachable and exec fallback disabled: %w", gwErr)
			}
		}

		atomic.AddInt64(&s.spawnCountExec, 1)

		provider := s.provider
		if project.Provider != "" {
			provider = project.Provider
		}

		args := []string{
			"chat", "-q", prompt,
			"-m", model,
			"--provider", provider,
			"-s", "coding-hermes-board",
			"-s", "coding-hermes-model-router",
			"-s", "coding-hermes-never-done",
			"-s", "coding-hermes-specs",
			"-s", "coding-hermes-testing",
			"-s", "coding-hermes-middle-out",
			"-s", "systematic-debugging",
			"-s", "trust-but-verify",
			"-s", "reality-validation",
			"-s", "github-pr-workflow",
			"-s", "github-repo-management",
			"-s", "claude-design",
			"-s", "popular-web-designs",
			"-s", "hilo",
			"-s", "gitreins-usage",
			"--ignore-rules", "-Q",
		}

		cmd = exec.Command("hermes", args...)
		cmd.Dir = project.Workdir
		cmd.Env = append(os.Environ(),
			"HERMES_HOME="+s.foremanHome,
			"CODING_HERMES_TICK="+tickID,
			"CODING_HERMES_SOURCE=scheduler",
			"CODING_HERMES_PROJECT="+project.Name,
		)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.noteSpawnFailure(project.Name)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		s.noteSpawnFailure(project.Name)
		return nil, fmt.Errorf("start process: %w", err)
	}

	s.mu.Lock()
	s.active[tickID] = cmd
	s.mu.Unlock()

	st := &SpawnedTick{
		TickID:     tickID,
		Project:    project.Name,
		PID:        cmd.Process.Pid,
		Started:    time.Now(),
		Deliver:    project.Deliver,
		cmd:        cmd,
		stdout:     stdout,
		stderr:     stderr,
		spawner:    s,
		preHead:    "",
		preCommits: -1,
	}

	// Snapshot the repo at spawn so the completion path can count commits and
	// files the foreman added during this tick.
	st.preHead, st.preCommits = gitBaseline(project.Workdir)

	// Tee stdout: scanner reads session_id from one side, buffer captures full output.
	teeReader := io.TeeReader(stdout, &st.Output)

	// Parse session ID from stdout and persist it. The scanner goroutine must
	// exit when the process exits or times out so it cannot leak.
	scanCtx, scanCancel := context.WithTimeout(context.Background(), s.timeout)
	st.scanCancel = scanCancel

	// Close stdout when context expires — unblocks scanner.Scan().
	go func() {
		<-scanCtx.Done()
		_ = stdout.Close()
	}()

	go func() {
		defer scanCancel()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("ERROR: stdout scanner panic for tick %s: %v", tickID, r)
			}
		}()
		scanner := bufio.NewScanner(teeReader)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "session_id:") {
				id := strings.TrimSpace(strings.TrimPrefix(line, "session_id:"))
				st.mu.Lock()
				st.SessionID = id
				st.mu.Unlock()
				// Persist session_id to the database.
				if _, err := s.db.Exec(`UPDATE ticks SET session_id = ? WHERE id = ?`, id, tickID); err != nil {
					log.Printf("ERROR persisting session_id for %s: %v", tickID, err)
				}
				continue
			}
		}
		if err := scanner.Err(); err != nil {
			// Expected on timeout (pipe closed) or process exit — not a leak.
			if !errors.Is(err, io.EOF) {
				log.Printf("WARN: stdout scanner error for tick %s: %v", tickID, err)
			}
		}
	}()

	// Update tick to running with PID for zombie detection.
	_, err = s.db.Exec(`
		UPDATE ticks SET status = 'running', spawned_at = ?, pid = ?
		WHERE id = ?
	`, st.Started.Format(time.RFC3339), st.PID, tickID)
	if err != nil {
		log.Printf("ERROR updating tick %s to running: %v", tickID, err)
	}
	// Also set last_tick_started on the project so cooldown tracking works.
	// S-GAP-001: a successful spawn resets the consecutive-failure backoff
	// counter (atomically with the last_tick_started write).
	_, _ = s.db.Exec(`UPDATE projects SET last_tick_started = ?, consecutive_failures = 0 WHERE name = ?`,
		st.Started.Format(time.RFC3339), project.Name)

	log.Printf("SPAWN: %s tick=%s pid=%d workdir=%s", project.Name, tickID, st.PID, project.Workdir)
	return st, nil
}

// SpawnedTick represents a running foreman process.
type SpawnedTick struct {
	TickID     string
	Project    string
	PID        int
	Started    time.Time
	SessionID  string
	Output     bytes.Buffer // full stdout for delivery after completion
	Deliver    string       // delivery target (telegram:chat_id:thread_id)
	cmd        *exec.Cmd
	stdout     interface{ Close() error }
	stderr     interface{ Close() error }
	spawner    *Spawner
	scanCancel context.CancelFunc
	mu         sync.Mutex

	// Git baseline captured at spawn (exec path only) so Wait() can measure
	// the commits/files the foreman produced during this tick. preCommits < 0
	// means the workdir was not a usable git repo at spawn.
	preHead    string
	preCommits int

	// completed is true for gateway-spawned ticks that finished in Spawn().
	completed  bool
	completeAt time.Time
}

// Wait blocks until the process exits and returns the outcome.
// For gateway-completed ticks (HTTP spawn), returns immediately.
func (st *SpawnedTick) Wait() TickOutcome {
	defer func() {
		st.spawner.mu.Lock()
		delete(st.spawner.active, st.TickID)
		st.spawner.mu.Unlock()
	}()

	// Gateway-spawned ticks are already complete — return immediately.
	if st.completed {
		return TickOutcome{
			TickID:    st.TickID,
			Project:   st.Project,
			SessionID: st.SessionID,
			Started:   st.Started,
			Finished:  st.completeAt,
			Status:    TickCompleted,
			Duration:  st.completeAt.Sub(st.Started),
		}
	}

	defer st.closePipes()
	if st.scanCancel != nil {
		defer st.scanCancel()
	}

	timer := time.AfterFunc(st.spawner.timeout, func() {
		if st.cmd.Process != nil {
			// Each scheduler-owned worker has its own process group. Killing the
			// group prevents shells, Hermes workers, and test runners from
			// surviving after the tick is marked timed out.
			_ = syscall.Kill(-st.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	defer timer.Stop()

	err := st.cmd.Wait()
	finished := time.Now()

	outcome := TickOutcome{
		TickID:    st.TickID,
		Project:   st.Project,
		SessionID: st.SessionID,
		Started:   st.Started,
		Finished:  finished,
	}

	if err != nil {
		if strings.Contains(err.Error(), "signal: killed") || strings.Contains(err.Error(), "killed") {
			outcome.Status = TickTimeout
		} else {
			outcome.Status = TickFailed
			outcome.Error = err.Error()
		}
	} else {
		outcome.Status = TickCompleted
	}

	outcome.ExitCode = st.cmd.ProcessState.ExitCode()
	outcome.Duration = finished.Sub(st.Started)

	// Cost: prefer REAL per-tick cost from the foreman's Hermes state.db
	// (session_model_usage.estimated/actual_cost_usd overlapping this tick's
	// window). Falls back to the flat estimate when telemetry is unavailable.
	// Populated on completed AND timed-out ticks: a timeout runs the full
	// window (killed at the cap), so it consumes a full tick's tokens and has a
	// real cost. Failed ticks that exit early consumed fewer and stay near 0.
	if outcome.Status == TickCompleted || outcome.Status == TickTimeout {
		workdir := ""
		if st.cmd != nil && st.cmd.Dir != "" {
			workdir = st.cmd.Dir
		}
		cost, isReal := resolveRealTickCost(st.spawner.foremanHome, workdir, st.Project, st.Started, finished)
		outcome.CostUSD = cost
		if !isReal {
			// Still record the estimated token counts so aggregation works
			// even when telemetry is missing.
			tin, tout, _ := estimateTickCost()
			outcome.TokensIn = tin
			outcome.TokensOut = tout
		} else {
			outcome.TokensIn = 0
			outcome.TokensOut = 0
		}
		// Measure real git work the foreman produced this tick (exec path only —
		// gateway spawns have no process/repo baseline). Best-effort: a non-git
		// or unreadable workdir leaves commits/files at 0.
		if st.preCommits >= 0 && st.cmd != nil && st.cmd.Dir != "" {
			outcome.Commits, outcome.FilesChanged = gitWorkDelta(st.cmd.Dir, st.preHead, st.preCommits)
		}
	}

	log.Printf("TICK: %s %s → %s (%v)", st.Project, st.TickID, outcome.Status, outcome.Duration.Round(time.Second))
	return outcome
}

func (st *SpawnedTick) closePipes() {
	if st.stdout != nil {
		_ = st.stdout.Close()
	}
	if st.stderr != nil {
		_ = st.stderr.Close()
	}
}

func splitCommand(cmd string) []string {
	// Simple split for shell commands. Does basic quote handling.
	var parts []string
	var current string
	inQuote := false
	for _, c := range cmd {
		switch c {
		case '"':
			inQuote = !inQuote
		case ' ':
			if inQuote {
				current += string(c)
			} else if current != "" {
				parts = append(parts, current)
				current = ""
			}
		default:
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
