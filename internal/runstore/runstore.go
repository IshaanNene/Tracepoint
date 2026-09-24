// Package runstore owns the on-disk run directory (§6.4): state, the append-only event
// log, the stop sentinel, status with lost-run detection, wait, list, gc and the audit
// log.
//
// A run is a resource on disk rather than a process, because a useful load test outlives
// an agent's tool call. Anything that can read the directory - another process, another
// tool, a human with cat - can follow it, and a run whose process died is recognised as
// lost rather than reported as running forever.
package runstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IshaanNene/Tracepoint/internal/clock"
	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// File names inside a run directory.
const (
	StateFile   = "state.json"
	EventsFile  = "events.ndjson"
	ResultFile  = "result.json"
	DigestFile  = "digest.json"
	ReportFile  = "report.html"
	ConfigFile  = "config.effective.yaml"
	LogFile     = "run.log"
	stopFile    = "STOP"
	auditFile   = "audit.ndjson"
	stateSchema = "1.0"
)

// Statuses. Terminal statuses never change once written.
const (
	StatusPending     = "pending"
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusInterrupted = "interrupted"
	StatusAborted     = "aborted"
	StatusFailed      = "failed"
	StatusLost        = "lost"
)

// Terminal reports whether a status is final.
func Terminal(status string) bool {
	switch status {
	case StatusPending, StatusRunning:
		return false
	default:
		return true
	}
}

// HeartbeatInterval is how often a live run refreshes its state, and StaleAfter is how
// long a silent heartbeat is tolerated before the run is checked for being lost.
const (
	HeartbeatInterval = 2 * time.Second
	StaleAfter        = 15 * time.Second
)

// State is state.json: what a run is doing, readable by anyone at any time.
type State struct {
	SchemaVersion string    `json:"schema_version"`
	RunID         string    `json:"run_id"`
	RunDir        string    `json:"run_dir"`
	Status        string    `json:"status"`
	Phase         string    `json:"phase,omitempty"`
	PID           int       `json:"pid,omitempty"`
	CreatedAt     string    `json:"created_at"`
	StartedAt     string    `json:"started_at,omitempty"`
	FinishedAt    string    `json:"finished_at,omitempty"`
	Heartbeat     string    `json:"heartbeat,omitempty"`
	Progress      *Progress `json:"progress,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	Error         any       `json:"error,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	ConfigHash    string    `json:"config_hash,omitempty"`
	Detached      bool      `json:"detached,omitempty"`
	StopRequested bool      `json:"stop_requested,omitempty"`
	Artifacts     Artifacts `json:"artifacts"`
}

// Progress is how far a live run has got.
type Progress struct {
	ElapsedMS float64 `json:"elapsed_ms"`
	TotalMS   float64 `json:"total_ms"`
	Ratio     float64 `json:"ratio"`
}

// Artifacts are the paths of a run's files, relative to the working directory.
type Artifacts struct {
	RunDir string `json:"run_dir"`
	State  string `json:"state"`
	Events string `json:"events"`
	Result string `json:"result"`
	Digest string `json:"digest"`
	Report string `json:"report"`
	Config string `json:"config"`
	Log    string `json:"log"`
}

// Store is a directory of runs.
type Store struct {
	root  string
	clk   clock.Clock
	alive func(pid int) bool
}

// Open prepares a store rooted at root, creating it if needed.
func Open(root string, clk clock.Clock) (*Store, error) {
	if root == "" {
		root = "runs"
	}
	if clk == nil {
		clk = clock.New()
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, errs.Wrap(errs.CodeIOWriteFailed, err, "creating the run root %s", root)
	}
	return &Store{root: root, clk: clk, alive: processAlive}, nil
}

// Root is the directory runs live under.
func (s *Store) Root() string { return s.root }

// SetProcessCheck replaces the liveness check, for tests.
func (s *Store) SetProcessCheck(f func(pid int) bool) { s.alive = f }

// Run is one run directory.
type Run struct {
	ID    string
	Dir   string
	store *Store

	mu sync.Mutex
}

// Create makes a new run directory. It refuses to reuse one: a run id names exactly one
// run, forever.
func (s *Store) Create(runID string) (*Run, error) {
	dir := filepath.Join(s.root, runID)
	return s.CreateAt(runID, dir)
}

// CreateAt makes a run directory at an explicit path, for --out-dir.
func (s *Store) CreateAt(runID, dir string) (*Run, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return nil, errs.Wrap(errs.CodeIOWriteFailed, err, "creating %s", filepath.Dir(dir))
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errs.New(errs.CodeIOWriteFailed, "the run directory %s already exists", dir).
				WithHint("run directories are never reused; choose another --out-dir")
		}
		return nil, errs.Wrap(errs.CodeIOWriteFailed, err, "creating the run directory %s", dir)
	}
	r := &Run{ID: runID, Dir: dir, store: s}
	now := s.clk.Now().UTC().Format(time.RFC3339Nano)
	st := State{
		SchemaVersion: stateSchema, RunID: runID, RunDir: dir, Status: StatusPending,
		CreatedAt: now, Heartbeat: now, PID: os.Getpid(), Artifacts: r.Artifacts(),
	}
	if err := r.WriteState(st); err != nil {
		return nil, err
	}
	return r, nil
}

// Path joins a file name onto the run directory.
func (r *Run) Path(name string) string { return filepath.Join(r.Dir, name) }

// Artifacts lists the run's file paths.
func (r *Run) Artifacts() Artifacts {
	return Artifacts{
		RunDir: r.Dir, State: r.Path(StateFile), Events: r.Path(EventsFile),
		Result: r.Path(ResultFile), Digest: r.Path(DigestFile), Report: r.Path(ReportFile),
		Config: r.Path(ConfigFile), Log: r.Path(LogFile),
	}
}

// WriteState replaces state.json atomically, so a reader never sees half a document.
func (r *Run) WriteState(st State) error {
	st.SchemaVersion = stateSchema
	st.RunID, st.RunDir = r.ID, r.Dir
	st.Artifacts = r.Artifacts()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "encoding the run state")
	}
	return writeAtomic(r.Path(StateFile), append(data, '\n'))
}

// UpdateState reads, changes and writes state.json. Updates from one process are
// serialised; a terminal status is never overwritten.
func (r *Run) UpdateState(change func(*State)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, err := r.ReadState()
	if err != nil {
		return err
	}
	wasTerminal := Terminal(st.Status)
	prev := st.Status
	change(&st)
	if wasTerminal {
		st.Status = prev
	}
	return r.WriteState(st)
}

// ReadState reads state.json as written, without lost-run detection.
func (r *Run) ReadState() (State, error) {
	var st State
	data, err := os.ReadFile(r.Path(StateFile))
	if err != nil {
		return st, errs.Wrap(errs.CodeIOReadFailed, err, "reading the state of run %s", r.ID)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, errs.Wrap(errs.CodeIOReadFailed, err, "decoding the state of run %s", r.ID)
	}
	return st, nil
}

// Status reads state.json and reports a run whose heartbeat went stale and whose
// process is gone as lost: it died without writing its result, and waiting for it
// would wait forever.
func (r *Run) Status() (State, error) {
	st, err := r.ReadState()
	if err != nil {
		return st, err
	}
	if Terminal(st.Status) {
		return st, nil
	}
	if _, statErr := os.Stat(r.Path(stopFile)); statErr == nil {
		st.StopRequested = true
	}
	hb, perr := time.Parse(time.RFC3339Nano, st.Heartbeat)
	if perr != nil {
		return st, nil
	}
	if r.store.clk.Now().Sub(hb) > StaleAfter && (st.PID == 0 || !r.store.alive(st.PID)) {
		st.Status = StatusLost
		st.Error = errs.EnvelopeOf(errs.New(errs.CodeRunLost,
			"run %s stopped reporting at %s and its process is gone", r.ID, st.Heartbeat).
			WithHint("it died without writing a result; start it again"))
	}
	return st, nil
}

// OpenEvents opens events.ndjson for appending. The caller owns the file.
func (r *Run) OpenEvents() (*os.File, error) {
	f, err := os.OpenFile(r.Path(EventsFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.CodeIOWriteFailed, err, "opening the event log of run %s", r.ID)
	}
	return f, nil
}

// RequestStop writes the STOP sentinel. The run polls for it and stops gracefully,
// which works the same on every platform, where signals do not.
func (r *Run) RequestStop() error {
	if err := os.WriteFile(r.Path(stopFile), []byte("stop\n"), 0o600); err != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing the stop request for run %s", r.ID)
	}
	return nil
}

// StopRequested reports whether the sentinel is present.
func (r *Run) StopRequested() bool {
	_, err := os.Stat(r.Path(stopFile))
	return err == nil
}

// WriteFile writes an artifact into the run directory atomically.
func (r *Run) WriteFile(name string, data []byte) error {
	return writeAtomic(r.Path(name), data)
}

// Get opens an existing run by id, by unique id prefix, or by the path of its
// directory.
func (s *Store) Get(ref string) (*Run, error) {
	if ref == "" {
		return nil, errs.New(errs.CodeRunNotFound, "no run was named").
			WithHint("pass a run id; `tracepoint list` shows them")
	}
	if info, err := os.Stat(filepath.Join(ref, StateFile)); err == nil && !info.IsDir() {
		return &Run{ID: filepath.Base(filepath.Clean(ref)), Dir: filepath.Clean(ref), store: s}, nil
	}
	if strings.ContainsAny(ref, `/\`) || ref == "." || ref == ".." {
		return nil, errs.New(errs.CodeRunNotFound, "no run directory at %s", ref)
	}
	if _, err := os.Stat(filepath.Join(s.root, ref, StateFile)); err == nil {
		return &Run{ID: ref, Dir: filepath.Join(s.root, ref), store: s}, nil
	}
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, ref) || strings.HasSuffix(id, "-"+ref) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 1:
		return &Run{ID: matches[0], Dir: filepath.Join(s.root, matches[0]), store: s}, nil
	case 0:
		return nil, errs.New(errs.CodeRunNotFound, "no run %q under %s", ref, s.root).
			WithHint("`tracepoint list` shows the runs; pass --run-root if they live elsewhere")
	default:
		return nil, errs.New(errs.CodeRunNotFound, "%q matches %d runs", ref, len(matches)).
			WithHint("give more of the id: %s", strings.Join(matches, ", "))
	}
}

// ids lists run directories, oldest first. Run ids begin with a UTC timestamp, so
// lexical order is chronological.
func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, errs.Wrap(errs.CodeIOReadFailed, err, "listing %s", s.root)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, e.Name(), StateFile)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// List returns every run's status, newest first.
func (s *Store) List() ([]State, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	out := make([]State, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		r := &Run{ID: ids[i], Dir: filepath.Join(s.root, ids[i]), store: s}
		st, err := r.Status()
		if err != nil {
			continue // a directory mid-creation or damaged; it is not a run to report
		}
		out = append(out, st)
	}
	return out, nil
}

// Active lists runs that are pending or running and not lost.
func (s *Store) Active() ([]State, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []State
	for _, st := range all {
		if !Terminal(st.Status) {
			out = append(out, st)
		}
	}
	return out, nil
}

// GC removes finished runs beyond the newest keep. Pending and running runs are never
// removed, whatever their age.
func (s *Store) GC(keep int) ([]string, error) {
	if keep < 0 {
		return nil, errs.New(errs.CodeOpsInvalidInput, "--keep must not be negative")
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	finished := 0
	for _, st := range all {
		if !Terminal(st.Status) {
			continue
		}
		finished++
		if finished <= keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, st.RunID)); err != nil {
			return removed, errs.Wrap(errs.CodeIOWriteFailed, err, "removing run %s", st.RunID)
		}
		removed = append(removed, st.RunID)
	}
	return removed, nil
}

// AuditRecord is one line of the audit log: who ran what, against what, how hard, and
// how it ended.
type AuditRecord struct {
	Time       string             `json:"time"`
	Event      string             `json:"event"`
	RunID      string             `json:"run_id"`
	Actor      string             `json:"actor"`
	ConfigHash string             `json:"config_hash,omitempty"`
	Targets    []string           `json:"targets,omitempty"`
	PeakRates  map[string]float64 `json:"peak_rates,omitempty"`
	Status     string             `json:"status,omitempty"`
	ExitCode   *int               `json:"exit_code,omitempty"`
	Validity   string             `json:"validity,omitempty"`
	Bottleneck string             `json:"bottleneck,omitempty"`
	Error      string             `json:"error,omitempty"`
}

// Audit appends one record to <root>/audit.ndjson. Each record is written with a single
// append, which POSIX keeps whole even with several writers.
func (s *Store) Audit(rec AuditRecord) error {
	if rec.Time == "" {
		rec.Time = s.clk.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "encoding an audit record")
	}
	f, err := os.OpenFile(filepath.Join(s.root, auditFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, err, "opening the audit log")
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	if werr != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, werr, "writing the audit log")
	}
	if cerr != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, cerr, "closing the audit log")
	}
	return nil
}

// AuditPath is where the audit log lives.
func (s *Store) AuditPath() string { return filepath.Join(s.root, auditFile) }

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", path)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(name)
		return errs.Wrap(errs.CodeIOWriteFailed, errors.Join(werr, cerr), "writing %s", path)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", path)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return errs.Wrap(errs.CodeIOWriteFailed, err, "writing %s", path)
	}
	return nil
}
