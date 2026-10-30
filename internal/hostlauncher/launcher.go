// Package hostlauncher is the root-installed program a host-process node runs
// every job through (ADR 0090).
//
// The daemon never executes a job, never touches a job's files, and never
// signals a job's processes itself. It runs exactly one argument vector,
//
//	sudo -n -u <runnerUser> -- /usr/local/libexec/tart-runner-fleet-host-launcher <verb> ...
//
// and a sudoers drop-in admits that one program as that one user. Everything a
// job can reach therefore belongs to the runner user, and nothing the daemon's
// user owns — the GitHub App key, `fleet.db`, other runners' JIT configurations
// — is readable from inside a job.
//
// The verbs mirror the executor port, so the adapter above is a translation and
// not a second state machine:
//
//	create <name> <image> <controller> <resource> <operation>   clone the runner template
//	start <name> <operation>                                    spawn the supervisor
//	exec <name> true                                            readiness and liveness
//	exec -i <name> <helper> [--require-capabilities=...]        hand over the JIT on stdin
//	stop <name> <operation> <seconds>                           SIGTERM, then SIGKILL, the group
//	delete <name> <operation>                                   remove a stopped instance
//	list                                                        every instance, as JSON
//
// An instance is a directory under the configured root. Its power is the state
// of one advisory lock: the supervisor takes it and passes the descriptor to the
// runner and so to every process the job starts, and the kernel drops it when
// the last of them exits. Reading it needs no PID file and cannot be fooled by a
// recycled PID; a lock that cannot be read is PowerUnknown, never "stopped".
package hostlauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/domain"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/guestbootstrap"
)

// Exit statuses are the launcher's whole vocabulary toward the daemon. Its
// standard error is a human aid; the adapter classifies on these numbers alone,
// so no text a job could influence decides a destructive step. 3 and 4 are the
// guest helper's capability statuses, reused so the lifecycle's existing
// classifier reads both backends' bootstrap failures identically.
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitNotFound = 5
	ExitConflict = 6
	ExitRunning  = 7
)

// Power is an instance's process state as the launcher read it.
type Power string

const (
	PowerRunning Power = "running"
	PowerStopped Power = "stopped"
	PowerUnknown Power = "unknown"
)

// Entry is one row of `list`.
type Entry struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Operation string `json:"operation"`
	Power     Power  `json:"power"`
}

// ConfigPath is where the setup script installs the launcher's root-owned
// configuration.
const ConfigPath = "/usr/local/etc/tart-runner-fleet/host-launcher.json"

const (
	defaultRunnerPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	maxJITBytes       = 1 << 20
	stagingPrefix     = ".staging-"
	cloneTimeout      = 10 * time.Minute
)

// ownershipGrammar bounds the durable ownership triple the marker records. The
// values come from the daemon, are visible in `ps`, and are never secret; the
// grammar only keeps them printable and unambiguous in one argument vector.
var ownershipGrammar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+=-]{0,255}$`)

// Config is the launcher's root-owned configuration. It is the half of the
// host-process contract the daemon's user cannot change: where instances live,
// what they are cloned from, and whose account and home a job runs with.
type Config struct {
	RunnerUser     string   `json:"runnerUser"`
	RunnerUID      int      `json:"runnerUid"`
	RunnerHome     string   `json:"runnerHome"`
	InstanceRoot   string   `json:"instanceRoot"`
	RunnerTemplate string   `json:"runnerTemplate"`
	CloneCommand   []string `json:"cloneCommand"`
	// Path is the runner's PATH. Empty is the Homebrew-first default.
	Path string `json:"path,omitempty"`
	// CapabilityManifest is the host's declaration of what it provides, read
	// exactly as a guest's sealed manifest is (ADR 0090's tool audit).
	CapabilityManifest string `json:"capabilityManifest"`
}

// Launcher is one invocation. Every dependency on the process and the clock is
// a field so the verbs are testable without a second user or a real signal.
type Launcher struct {
	Config Config
	// ConfigErr is why the root configuration could not be loaded; a launcher
	// with one refuses every verb.
	ConfigErr      error
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getuid         func() int
	Getenv         func(string) string
	Kill           func(int, syscall.Signal) error
	// Spawn starts `<self> supervise <name>` detached in its own session.
	Spawn        func([]string) error
	PollInterval time.Duration
	StartTimeout time.Duration
	KillWait     time.Duration
}

// Production is the launcher main runs.
func Production() *Launcher {
	cfg, err := loadConfig(ConfigPath, 0)
	return &Launcher{Config: cfg, ConfigErr: err, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getuid: os.Getuid, Getenv: os.Getenv, Kill: syscall.Kill, Spawn: spawnDetached,
		PollInterval: 50 * time.Millisecond, StartTimeout: 10 * time.Second, KillWait: 10 * time.Second}
}

// executable and rename are the two process facts a test cannot otherwise
// stage: which binary is running, and a rename that loses a race.
var (
	executable = os.Executable
	rename     = os.Rename
)

// spawnDetached re-executes the launcher in a new session, so the supervisor is
// the leader of the process group stop signals and outlives the sudo that
// started it. It is deliberately not bound to a context: it must survive this
// invocation.
func spawnDetached(args []string) error {
	self, err := executable()
	if err != nil {
		return err
	}
	// #nosec G204 -- the launcher re-executes itself with a vector it validated.
	command := exec.Command(self, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// loadConfig reads the configuration and refuses one the runner user or the
// daemon's user could have written: it must belong to owner (root in
// production) and be writable by nobody else.
func loadConfig(path string, owner uint32) (Config, error) {
	info, statErr := os.Lstat(path)
	body, readErr := os.ReadFile(path) // #nosec G304 -- a fixed root-owned path.
	if err := errors.Join(statErr, readErr); err != nil {
		return Config{}, fmt.Errorf("launcher configuration: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != owner || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return Config{}, errors.New("launcher configuration must be a regular file owned by root and writable by nobody else")
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, errors.New("launcher configuration is not valid JSON")
	}
	return cfg, cfg.validate()
}

func (c Config) validate() error {
	if c.RunnerUser == "" || c.RunnerUID <= 0 {
		return errors.New("launcher configuration needs a non-root runnerUser and runnerUid")
	}
	for _, path := range []string{c.RunnerHome, c.InstanceRoot, c.RunnerTemplate, c.CapabilityManifest} {
		if !cleanAbsolute(path) {
			return errors.New("launcher configuration paths must be clean and absolute")
		}
	}
	if len(c.CloneCommand) == 0 || !cleanAbsolute(c.CloneCommand[0]) {
		return errors.New("launcher configuration needs an absolute cloneCommand")
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

// errExit carries an exit status out of a verb.
type errExit struct {
	code int
	msg  string
}

func (e errExit) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return errExit{code: code, msg: fmt.Sprintf(format, args...)}
}

// Main runs one verb and returns its exit status.
func (l *Launcher) Main(args []string) int {
	err := l.dispatch(args)
	if err == nil {
		return ExitOK
	}
	_, _ = fmt.Fprintln(l.Stderr, "tart-runner-fleet-host-launcher:", err.Error())
	var exit errExit
	if errors.As(err, &exit) {
		return exit.code
	}
	var capability *guestbootstrap.CapabilityError
	if errors.As(err, &capability) {
		if capability.Unverifiable() {
			return guestbootstrap.ExitCapabilityUnverifiable
		}
		return guestbootstrap.ExitCapabilityMissing
	}
	return ExitFailure
}

func (l *Launcher) dispatch(args []string) error {
	if err := l.checkIdentity(); err != nil {
		return err
	}
	if len(args) == 0 {
		return fail(ExitUsage, "usage: create|start|exec|stop|delete|list")
	}
	verb, rest := args[0], args[1:]
	if verb == "list" && len(rest) == 0 {
		return l.list()
	}
	if verb == "exec" {
		return l.exec(rest)
	}
	arity := map[string]int{"create": 5, "start": 2, "supervise": 1, "stop": 3, "delete": 2}
	if want, known := arity[verb]; !known || len(rest) != want {
		return fail(ExitUsage, "usage: unknown verb or wrong argument count")
	}
	if domain.ValidateInstanceName(rest[0]) != nil {
		return fail(ExitUsage, "invalid instance name")
	}
	switch verb {
	case "create":
		return l.create(rest[0], rest[1], rest[2:])
	case "start":
		return l.start(rest[0], rest[1])
	case "supervise":
		return l.supervise(rest[0])
	case "stop":
		seconds, err := strconv.Atoi(rest[2])
		if err != nil || seconds < 0 || seconds > 3600 {
			return fail(ExitUsage, "stop grace must be 0..3600 seconds")
		}
		return l.stop(rest[0], rest[1], time.Duration(seconds)*time.Second)
	default:
		return l.delete(rest[0], rest[1])
	}
}

// checkIdentity refuses to act as anyone but the configured runner user, and
// refuses a daemon that is itself the runner user: that daemon would hand jobs
// its own credentials, which is the one thing this program exists to prevent.
func (l *Launcher) checkIdentity() error {
	if l.ConfigErr != nil {
		return l.ConfigErr
	}
	uid := l.Getuid()
	if uid == 0 || uid != l.Config.RunnerUID {
		return errors.New("the launcher runs only as the configured runner user")
	}
	if invoker := l.Getenv("SUDO_UID"); invoker != "" && invoker == strconv.Itoa(uid) {
		return errors.New("the daemon must not run as the runner user")
	}
	return nil
}

func (l *Launcher) dir(name string) string { return filepath.Join(l.Config.InstanceRoot, name) }

func (l *Launcher) control(name, file string) string {
	return filepath.Join(l.dir(name), "control", file)
}

type marker struct {
	Image      string `json:"image"`
	Controller string `json:"controller"`
	Resource   string `json:"resource"`
	Operation  string `json:"operation"`
}

func readMarker(dir string) (marker, error) {
	body, err := os.ReadFile(filepath.Join(dir, "ownership.json")) // #nosec G304 -- inside the instance root.
	var m marker
	if err != nil || json.Unmarshal(body, &m) != nil || m.Operation == "" {
		return marker{}, errors.New("ownership marker is unreadable")
	}
	return m, nil
}

// owned resolves an instance this operation created. An absent directory is
// ExitNotFound; a directory whose marker names another operation, or none, is a
// conflict, because the launcher acts only on what the asking operation made.
func (l *Launcher) owned(name, operation string) error {
	info, err := os.Lstat(l.dir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return fail(ExitNotFound, "instance %s does not exist", name)
	}
	if err != nil || !info.IsDir() {
		return fail(ExitFailure, "instance %s cannot be inspected", name)
	}
	m, err := readMarker(l.dir(name))
	if err != nil || m.Operation != operation {
		return fail(ExitConflict, "instance %s was not created by operation %s", name, operation)
	}
	return nil
}

func (l *Launcher) create(name, image string, ownership []string) error {
	if image != l.Config.RunnerTemplate {
		return fail(ExitUsage, "image is not this launcher's runner template")
	}
	for _, value := range ownership {
		if !ownershipGrammar.MatchString(value) {
			return fail(ExitUsage, "invalid ownership value")
		}
	}
	if err := l.owned(name, ownership[2]); err == nil || !isExit(err, ExitNotFound) {
		return err
	}
	// A staging directory is only ever this launcher's own, left by a create
	// that crashed; it is cleared, and anything that cannot be cleared fails the
	// Mkdir below. The rename publishes the instance whole or not at all.
	staging := filepath.Join(l.Config.InstanceRoot, stagingPrefix+name)
	_ = removeTree(staging)
	err := l.stage(staging, image, ownership)
	if err == nil {
		err = rename(staging, l.dir(name))
	}
	if err != nil {
		_ = removeTree(staging)
	}
	return err
}

func (l *Launcher) stage(staging, image string, ownership []string) error {
	for _, dir := range []string{staging, filepath.Join(staging, "control"), filepath.Join(staging, "tmp")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	argv := append(append([]string(nil), l.Config.CloneCommand[1:]...), image, filepath.Join(staging, "runner"))
	// #nosec G204 -- the clone command comes from the root-owned configuration.
	if err := exec.CommandContext(ctx, l.Config.CloneCommand[0], argv...).Run(); err != nil {
		return fail(ExitFailure, "cloning the runner template failed")
	}
	body, _ := json.Marshal(marker{Image: image, Controller: ownership[0], Resource: ownership[1], Operation: ownership[2]})
	return os.WriteFile(filepath.Join(staging, "ownership.json"), body, 0o600)
}

func isExit(err error, code int) bool {
	var exit errExit
	return errors.As(err, &exit) && exit.code == code
}

// power reads the alive lock. An instance whose lock file was never created has
// never been started and is stopped; any other failure to read it is unknown.
func (l *Launcher) power(name string) Power {
	file, err := os.OpenFile(l.control(name, "alive.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return PowerStopped
	}
	if err == nil {
		defer func() { _ = file.Close() }()
		// Closing the descriptor releases a lock this probe took.
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- a descriptor fits an int.
		if err == nil {
			return PowerStopped
		}
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return PowerRunning
	}
	return PowerUnknown
}

func (l *Launcher) knownPower(name string) (Power, error) {
	power := l.power(name)
	if power == PowerUnknown {
		return power, fail(ExitFailure, "instance %s power cannot be read", name)
	}
	return power, nil
}

func (l *Launcher) start(name, operation string) error {
	if err := l.owned(name, operation); err != nil {
		return err
	}
	power, err := l.knownPower(name)
	if err != nil || power == PowerRunning {
		return err
	}
	if _, err := os.Lstat(l.control(name, "booted")); err == nil {
		return fail(ExitConflict, "instance %s already ran; an ephemeral instance never runs twice", name)
	}
	if err := l.Spawn([]string{"supervise", name}); err != nil {
		return fail(ExitFailure, "spawning the supervisor failed")
	}
	if !l.waitFor(l.StartTimeout, func() bool { return l.power(name) == PowerRunning && isFIFO(l.control(name, "jit.fifo")) }) {
		return fail(ExitFailure, "the supervisor of %s did not come up", name)
	}
	return nil
}

func isFIFO(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&fs.ModeNamedPipe != 0
}

func (l *Launcher) waitFor(limit time.Duration, done func() bool) bool {
	deadline := time.Now().Add(limit)
	for {
		if done() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(l.PollInterval)
	}
}

// supervise holds the instance for its whole life: it takes the alive lock,
// waits on the FIFO for the JIT configuration, and runs the runner with the lock
// descriptor inherited, so the instance reads as running until the last process
// of the job is gone.
func (l *Launcher) supervise(name string) error {
	lock, err := os.OpenFile(l.control(name, "alive.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if !l.waitFor(l.StartTimeout, func() bool {
		return syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil // #nosec G115 -- a descriptor fits an int.
	}) {
		return fail(ExitFailure, "instance %s is already held", name)
	}
	booted := l.control(name, "booted")
	if _, err := os.Lstat(booted); err == nil {
		return fail(ExitConflict, "instance %s already ran", name)
	}
	fifo := l.control(name, "jit.fifo")
	if err := errors.Join(os.WriteFile(booted, nil, 0o600),
		os.WriteFile(l.control(name, "pgid"), []byte(strconv.Itoa(syscall.Getpgrp())+"\n"), 0o600),
		syscall.Mkfifo(fifo, 0o600)); err != nil {
		return err
	}
	jit, err := readFIFO(fifo)
	if err != nil {
		return fail(ExitFailure, "invalid runner configuration on the FIFO")
	}
	return l.runRunner(name, jit, lock)
}

// readFIFO blocks until the bootstrap writes, and removes the FIFO whatever
// happens, so a second writer can never reach a supervisor that already read.
// A nil file reads as an error, which ReadJIT reports.
func readFIFO(path string) (string, error) {
	file, openErr := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- inside the instance root.
	_ = os.Remove(path)
	defer func() { _ = file.Close() }()
	jit, readErr := guestbootstrap.ReadJIT(file, maxJITBytes)
	return jit, errors.Join(openErr, readErr)
}

func (l *Launcher) runRunner(name, jit string, lock *os.File) error {
	log, err := os.OpenFile(l.control(name, "runner.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	runnerDir := filepath.Join(l.dir(name), "runner")
	// #nosec G204 -- the runner is the cloned template's own entry point.
	command := exec.Command(filepath.Join(runnerDir, "run.sh"))
	command.Dir = runnerDir
	command.Env = l.Config.runnerEnvironment(jit, filepath.Join(l.dir(name), "tmp"))
	command.Stdout, command.Stderr = log, log
	command.ExtraFiles = []*os.File{lock}
	if err := command.Start(); err != nil {
		return fail(ExitFailure, "starting the runner failed")
	}
	err = os.WriteFile(l.control(name, "started"), nil, 0o600)
	_ = command.Wait()
	return err
}

// runnerEnvironment is built, never inherited: the launcher runs under sudo
// with whatever the daemon's sudo policy kept, and none of it belongs in a job.
func (c Config) runnerEnvironment(jit, tmp string) []string {
	path := c.Path
	if path == "" {
		path = defaultRunnerPath
	}
	return []string{guestbootstrap.JITEnvironment + "=" + jit, "HOME=" + c.RunnerHome, "USER=" + c.RunnerUser,
		"LOGNAME=" + c.RunnerUser, "PATH=" + path, "TMPDIR=" + tmp, "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8"}
}

// exec accepts exactly the two vectors the lifecycle sends a guest: the
// readiness probe and the JIT bootstrap. Anything else is a usage error, so the
// launcher can never become a way to run an arbitrary command.
func (l *Launcher) exec(args []string) error {
	if len(args) == 2 && args[1] == "true" && domain.ValidateInstanceName(args[0]) == nil {
		if l.power(args[0]) != PowerRunning {
			return fail(ExitFailure, "instance %s is not running", args[0])
		}
		return nil
	}
	if len(args) < 3 || len(args) > 4 || args[0] != "-i" || args[2] != guestbootstrap.HelperPath ||
		domain.ValidateInstanceName(args[1]) != nil {
		return fail(ExitUsage, "exec accepts only the readiness probe and the bootstrap helper")
	}
	var required []string
	if len(args) == 4 {
		value, found := strings.CutPrefix(args[3], guestbootstrap.CapabilityFlag+"=")
		capabilities, err := guestbootstrap.ParseCapabilityList(value)
		if !found || err != nil {
			return fail(ExitUsage, "invalid capability argument")
		}
		required = capabilities
	}
	return l.bootstrap(args[1], required)
}

func (l *Launcher) bootstrap(name string, required []string) error {
	if l.power(name) != PowerRunning {
		return fail(ExitFailure, "instance %s is not running", name)
	}
	if err := guestbootstrap.CheckCapabilities(l.Config.CapabilityManifest, required); err != nil {
		return err
	}
	jit, err := guestbootstrap.ReadJIT(l.Stdin, maxJITBytes)
	if err != nil {
		return fail(ExitFailure, "invalid runner configuration on stdin")
	}
	if err := writeFIFO(l.control(name, "jit.fifo"), jit); err != nil {
		return fail(ExitFailure, "instance %s has no supervisor waiting for its configuration", name)
	}
	if !l.waitFor(l.StartTimeout, func() bool { _, err := os.Lstat(l.control(name, "started")); return err == nil }) {
		return fail(ExitFailure, "the runner of %s did not start", name)
	}
	return nil
}

// writeFIFO hands the JIT to a waiting supervisor. Opening without blocking
// fails when no reader is there, and the descriptor is proved to be a FIFO
// before a byte is written, so a regular file planted at the path never
// receives the secret.
func writeFIFO(path, jit string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) // #nosec G304 -- inside the instance root.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
		return errors.New("not a FIFO")
	}
	_, err = io.WriteString(file, jit+"\n")
	return err
}

// stop ends the instance's process group: SIGTERM and a grace period, then
// SIGKILL. A grace of zero is Terminate. An absent or already stopped instance
// is success; a group still holding the lock after SIGKILL is ExitRunning, so
// the daemon's stop ladder escalates rather than believing it.
func (l *Launcher) stop(name, operation string, grace time.Duration) error {
	if err := l.owned(name, operation); err != nil {
		if isExit(err, ExitNotFound) {
			return nil
		}
		return err
	}
	power, err := l.knownPower(name)
	if err != nil || power == PowerStopped {
		return err
	}
	group, err := l.processGroup(name)
	if err != nil {
		return err
	}
	stopped := func() bool { return l.power(name) == PowerStopped }
	if grace > 0 {
		l.signal(group, syscall.SIGTERM)
		if l.waitFor(grace, stopped) {
			return nil
		}
	}
	l.signal(group, syscall.SIGKILL)
	if l.waitFor(l.KillWait, stopped) {
		return nil
	}
	return fail(ExitRunning, "instance %s is still running after SIGKILL", name)
}

func (l *Launcher) signal(group int, signal syscall.Signal) {
	if err := l.Kill(-group, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_, _ = fmt.Fprintln(l.Stderr, "tart-runner-fleet-host-launcher: signal:", err)
	}
}

func (l *Launcher) processGroup(name string) (int, error) {
	body, err := os.ReadFile(l.control(name, "pgid")) // #nosec G304 -- inside the instance root.
	group, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || parseErr != nil || group <= 1 {
		return 0, fail(ExitFailure, "instance %s names no process group", name)
	}
	return group, nil
}

func (l *Launcher) delete(name, operation string) error {
	if err := l.owned(name, operation); err != nil {
		if isExit(err, ExitNotFound) {
			return nil
		}
		return err
	}
	power, err := l.knownPower(name)
	if err != nil {
		return err
	}
	if power == PowerRunning {
		return fail(ExitRunning, "instance %s is running", name)
	}
	return removeTree(l.dir(name))
}

// removeTree deletes a directory a job wrote, including the parts it made
// unwritable to its own owner. It never follows a symbolic link.
func removeTree(root string) error {
	// WalkDir visits a directory before it reads it, so making each one
	// owner-accessible on the visit is what lets the walk descend into it.
	// The walk is scoped to the instance with os.Root, so a symbolic link a job
	// left behind can never send a chmod outside it.
	if scoped, err := os.OpenRoot(root); err == nil {
		_ = fs.WalkDir(scoped.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = scoped.Chmod(path, 0o700) // #nosec G302 -- the owner's own directory, about to be removed.
			}
			return nil
		})
		_ = scoped.Close()
	}
	return os.RemoveAll(root)
}

func (l *Launcher) list() error {
	entries, err := os.ReadDir(l.Config.InstanceRoot)
	if err != nil {
		return fail(ExitFailure, "the instance root cannot be read")
	}
	rows := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || domain.ValidateInstanceName(name) != nil || entry.Type() != fs.ModeDir {
			continue
		}
		row := Entry{Name: name, Power: l.power(name)}
		if m, err := readMarker(l.dir(name)); err == nil {
			row.Image, row.Operation = m.Image, m.Operation
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return json.NewEncoder(l.Stdout).Encode(rows)
}
