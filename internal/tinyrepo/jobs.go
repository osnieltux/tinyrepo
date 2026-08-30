package tinyrepo

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A job is one of the actions the flags perform, run from the panel instead.
//
// One at a time, on purpose: -di, -ci, -dp and -cl all read and write the same
// destination directory, and two of them at once would race over the same
// files. The queue is the same serialisation the command line gets for free by
// being a single process.

type jobKind string

const (
	jobFetchIndexes jobKind = "di"
	jobBuild        jobKind = "ci"
	jobDownloadPool jobKind = "dp"
	jobClean        jobKind = "cl"
)

// jobKinds is every action the panel offers, in the order the command line
// would normally run them.
var jobKinds = []jobKind{jobFetchIndexes, jobBuild, jobDownloadPool, jobClean}

func knownJob(kind string) bool {
	for _, k := range jobKinds {
		if string(k) == kind {
			return true
		}
	}
	return false
}

// maxJobLogLines bounds what a run keeps. A -dp over a large suite prints a
// line per package, and the panel is not a log server.
const maxJobLogLines = 500

// jobRun is a finished or running action.
type jobRun struct {
	Kind    jobKind
	Started time.Time
	Ended   time.Time
	Err     error
	Log     []string
	Running bool
}

// jobRunner serialises the actions and keeps the log of the current one.
type jobRunner struct {
	mu      sync.Mutex
	current *jobRun
	last    *jobRun

	// configPath is re-read before every run, so an action always uses what
	// the panel most recently saved rather than what -ws started with.
	configPath string
}

func newJobRunner(configPath string) *jobRunner {
	return &jobRunner{configPath: configPath}
}

// snapshot returns the run to display: the one in flight, or the last finished.
func (j *jobRunner) snapshot() *jobRun {
	j.mu.Lock()
	defer j.mu.Unlock()

	run := j.current
	if run == nil {
		run = j.last
	}
	if run == nil {
		return nil
	}

	// A copy, so the page renders a consistent state rather than one a worker
	// is appending to as the template walks it.
	clone := *run
	clone.Log = append([]string(nil), run.Log...)
	return &clone
}

func (j *jobRunner) busy() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.current != nil
}

// errJobBusy is returned when another action is already running.
var errJobBusy = errors.New("another action is already running")

// start launches an action unless one is already in flight.
func (j *jobRunner) start(kind jobKind) error {
	j.mu.Lock()
	if j.current != nil {
		j.mu.Unlock()
		return errJobBusy
	}
	run := &jobRun{Kind: kind, Started: time.Now(), Running: true}
	j.current = run
	j.mu.Unlock()

	go j.run(run)
	return nil
}

// appendLog adds a line to a run, dropping the oldest once the cap is reached.
func (j *jobRunner) appendLog(run *jobRun, line string) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if len(run.Log) == maxJobLogLines {
		run.Log = append(run.Log[:0], run.Log[1:]...)
	}
	run.Log = append(run.Log, time.Now().Format("15:04:05")+"  "+line)
}

func (j *jobRunner) run(run *jobRun) {
	defer func() {
		// An action is arbitrary work over data from a mirror; a panic in it
		// must take down the run, not the web server publishing the repository.
		if p := recover(); p != nil {
			j.appendLog(run, fmt.Sprintf("panic: %v", p))
			j.finish(run, fmt.Errorf("panic: %v", p))
		}
	}()

	config, err := loadConfigFile(j.configPath)
	if err != nil {
		j.appendLog(run, err.Error())
		j.finish(run, err)
		return
	}

	backend, err := newBackend(config.Server.Type)
	if err != nil {
		j.appendLog(run, err.Error())
		j.finish(run, err)
		return
	}

	j.appendLog(run, _t("job started")+" -"+string(run.Kind))

	switch run.Kind {
	case jobFetchIndexes:
		err = backend.FetchIndexes(config)
	case jobBuild:
		err = backend.Build(config)
	case jobDownloadPool:
		err = downloadPool(config)
	case jobClean:
		err = cleanRepo(config, backend)
	default:
		err = fmt.Errorf("%s: %s", _t("unknown action"), run.Kind)
	}

	if err != nil {
		j.appendLog(run, _t("job failed")+": "+err.Error())
	} else {
		j.appendLog(run, _t("job done"))
	}
	j.finish(run, err)
}

func (j *jobRunner) finish(run *jobRun, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	// finish runs from both the normal path and the deferred recover, so it has
	// to be idempotent: the first one to arrive is the one that counts.
	if !run.Running {
		return
	}
	run.Running = false
	run.Ended = time.Now()
	run.Err = err

	j.last = run
	j.current = nil
}

// Duration is how long a run took, or has been going.
func (r *jobRun) Duration() string {
	end := r.Ended
	if r.Running {
		end = time.Now()
	}
	return end.Sub(r.Started).Round(time.Second).String()
}

// Status is what the panel prints for a run.
func (r *jobRun) Status() string {
	switch {
	case r.Running:
		return "running"
	case r.Err != nil:
		return "failed"
	default:
		return "ok"
	}
}

// Description labels a run with what the flag does, in the reader's language.
func (r *jobRun) Description() string {
	return jobDescription(r.Kind)
}

func jobDescription(kind jobKind) string {
	switch kind {
	case jobFetchIndexes:
		return _t("download indexes")
	case jobBuild:
		return _t("create indexes")
	case jobDownloadPool:
		return _t("download packages")
	case jobClean:
		return _t("clean packages")
	}
	return string(kind)
}

// jobLogText renders a run's log as one block, for the panel's <pre>.
func jobLogText(run *jobRun) string {
	if run == nil {
		return ""
	}
	return strings.Join(run.Log, "\n")
}
