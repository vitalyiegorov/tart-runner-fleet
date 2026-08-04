package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vitalyiegorov/tart-runner-fleet/internal/adapters/githubscaleset"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/adminapi"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/app"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/autoupdate"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/config"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/credentials"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/hostpaths"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/operations"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/provision"
	"github.com/vitalyiegorov/tart-runner-fleet/internal/reconcile"
)

const (
	exitSuccess         = 0
	exitFailure         = 1
	exitUsage           = 2
	exitNotFound        = 3
	exitUnavailable     = 4
	exitDegraded        = 5
	exitUnsafe          = 6
	updateReadyAttempts = 150
	updateReadyDelay    = 2 * time.Second
)

// defaultVersion is reported when no build identity was injected, which is the
// case for `go run` and for tests.
const defaultVersion = "dev"

type apiClient interface {
	Status(context.Context) (adminapi.StatusEnvelope, error)
	Probe(context.Context, bool) (adminapi.Check, error)
	Metrics(context.Context) (string, error)
	Discharge(context.Context, adminapi.DischargeRequest) (adminapi.DischargeResult, error)
}

type dependencies struct {
	newClient                func(string, time.Duration) (apiClient, error)
	openConfig               func(string) (io.ReadCloser, error)
	loadPrivateKey           func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error)
	openProvision            func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error)
	openReconcilingProvision func(githubscaleset.GitHubAppAdminConfig) (provision.Client, error)
	writeConfig              func(string, config.Config) error
	command                  autoupdate.Command
	version                  string
}

// buildVersion reports the injected build identity, falling back to the
// development default when the CLI is constructed without one.
func (d dependencies) buildVersion() string {
	if d.version == "" {
		return defaultVersion
	}
	return d.version
}

type loadedSecret interface {
	Reveal() string
	Destroy()
}

type secretLoader func(context.Context, string, string, string) (loadedSecret, error)

func privateKeyLoader(load secretLoader) func(context.Context, string, string, string) (*githubscaleset.PrivateKeySecret, error) {
	return func(ctx context.Context, service, account, path string) (*githubscaleset.PrivateKeySecret, error) {
		secret, err := load(ctx, service, account, path)
		if err != nil {
			return nil, err
		}
		if secret == nil {
			return nil, operations.ErrInvalid
		}
		key := githubscaleset.NewPrivateKeySecret(secret.Reveal())
		secret.Destroy()
		return key, nil
	}
}

func defaultDependencies() dependencies {
	appKey := credentials.GitHubAppKey{}
	return dependencies{
		newClient: func(endpoint string, timeout time.Duration) (apiClient, error) {
			return adminapi.NewClient(endpoint, timeout)
		},
		openConfig: func(path string) (io.ReadCloser, error) {
			return os.Open(path) // #nosec G304 -- explicit operator-selected config path.
		},
		loadPrivateKey: privateKeyLoader(func(ctx context.Context, service, account, path string) (loadedSecret, error) {
			return appKey.Load(ctx, service, account, path)
		}),
		openProvision: func(cfg githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
			return githubscaleset.NewProvisioner(cfg)
		},
		openReconcilingProvision: func(cfg githubscaleset.GitHubAppAdminConfig) (provision.Client, error) {
			provisioner, err := githubscaleset.NewProvisioner(cfg)
			if err != nil {
				return nil, err
			}
			provisioner.ReconcileDrift = true
			return provisioner, nil
		},
		writeConfig: atomicWriteConfig,
		command:     execCommand{},
		version:     defaultVersion,
	}
}

// Execute runs the operator command surface. The caller owns process exit and
// signal handling; version is injected by the single fleet entry point so the
// CLI and the daemon can never report different builds.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	deps := defaultDependencies()
	deps.version = version
	return executeWith(ctx, args, stdout, stderr, deps)
}

func execute(args []string, stdout, stderr io.Writer) int {
	return executeWith(context.Background(), args, stdout, stderr, defaultDependencies())
}

func executeWith(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	connectionArgs, args := splitLeadingConnectionArgs(args)
	if len(args) == 0 {
		writeHelp(stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "--help", "-h":
		writeHelp(stdout)
		return exitSuccess
	case "version":
		return runVersion(args[1:], stdout, stderr, deps.buildVersion())
	case "api-version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: fleet api-version")
			return exitUsage
		}
		fmt.Fprintln(stdout, adminapi.APIVersion)
		return exitSuccess
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "scale-sets":
		return runScaleSets(ctx, args[1:], stdout, stderr, deps)
	case "update":
		return runUpdate(ctx, args[1:], stdout, stderr, deps)
	case "validate-config":
		return runConfig(append([]string{"validate"}, args[1:]...), stdout, stderr)
	case "operations":
		// `operations` stays the read-only view; only the explicit `discharge`
		// subcommand mutates, so the guarded path can never be reached by a typo in
		// an observation command.
		if len(args) > 1 && args[1] == "discharge" {
			return runDischarge(ctx, connectionArgs, args[2:], stdout, stderr, deps)
		}
		return runRemote(ctx, args[0], appendConnectionArgs(connectionArgs, args[1:]), stdout, stderr, deps)
	case "status", "queues", "instances", "observations", "health", "doctor", "metrics":
		return runRemote(ctx, args[0], appendConnectionArgs(connectionArgs, args[1:]), stdout, stderr, deps)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		writeHelp(stderr)
		return exitUsage
	}
}

func appendConnectionArgs(connectionArgs, args []string) []string {
	remoteArgs := make([]string, 0, len(connectionArgs)+len(args))
	remoteArgs = append(remoteArgs, connectionArgs...)
	return append(remoteArgs, args...)
}

// runDischarge is the only mutating operator command. It mirrors the guarded
// convention of `scale-sets provision`: an exact --confirm token, a non-empty
// --reason, and a fail-closed refusal. The daemon re-checks every guard, so this
// is defence in depth and not the safety boundary.
//
// --reap-instance is separate and additive because it is the destructive half: it
// retires the phantom's durable row and then removes its stopped VM, in that
// order. The reverse order would leave a live row owning an absent VM, which turns
// the whole instance observation Unavailable and blocks planning host-wide with no
// VM left to prove anything (see docs/OPERATIONS.md).
func runDischarge(ctx context.Context, connectionArgs, args []string, stdout, stderr io.Writer, deps dependencies) int {
	flags := flag.NewFlagSet("fleet operations discharge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	operation := flags.String("operation", "", "dead-lettered operation ID from `fleet operations`")
	instance := flags.String("instance", "", "owning instance ID the operation names")
	reap := flags.Bool("reap-instance", false, "also retire the instance row and delete its stopped VM")
	confirm := flags.String("confirm", "", "exact mutation confirmation")
	reason := flags.String("reason", "", "operator reason recorded in the audit log")
	opts, code := parseRemoteInto(flags, append(connectionArgs, args...), stderr)
	if code != exitSuccess {
		return code
	}
	if *operation == "" || *instance == "" {
		fmt.Fprintln(stderr, "usage: fleet operations discharge --operation ID --instance ID [--reap-instance] --confirm "+
			adminapi.DischargeConfirmation+" --reason \"text\"")
		return exitUsage
	}
	if *confirm != adminapi.DischargeConfirmation || strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "unsafe discharge: require --confirm "+adminapi.DischargeConfirmation+" and a non-empty --reason")
		return exitUnsafe
	}
	client, err := deps.newClient(opts.endpoint, opts.timeout)
	if err != nil {
		fmt.Fprintf(stderr, "connect: %v\n", err)
		return exitUnavailable
	}
	result, err := client.Discharge(ctx, adminapi.DischargeRequest{OperationID: *operation, InstanceID: *instance,
		ReapInstance: *reap, Confirm: *confirm, Reason: *reason})
	if err != nil {
		return dischargeError(stderr, err)
	}
	if opts.output == "json" {
		if err := writeJSON(stdout, result); err != nil {
			return exitFailure
		}
		return exitSuccess
	}
	fmt.Fprintf(stdout, "discharged %s\ninstance %s\noperation discharged %t\ninstance reaped %t\nvm deleted %t\n",
		result.OperationID, result.InstanceID, result.OperationDischarged, result.InstanceReaped, result.VMDeleted)
	return exitSuccess
}

// dischargeError reports the daemon's own bounded refusal code. A refused
// mutation is exit 6 (unsafe) so a script can never read it as success, except an
// unknown operation, which is exit 3 (not-found) because the operator simply named
// something that is not there.
func dischargeError(stderr io.Writer, err error) int {
	var refusal adminapi.Refusal
	if errors.As(err, &refusal) {
		fmt.Fprintf(stderr, "discharge refused: %s\n", refusal.Code)
		if refusal.Code == adminapi.RefusalUnknownOperation {
			return exitNotFound
		}
		return exitUnsafe
	}
	return remoteError(stderr, err)
}

type execCommand struct{}

func (execCommand) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput() // #nosec G204 -- arguments are validated release/update fields, never shell text.
}

func runUpdate(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) == 0 || (args[0] != "adopt" && args[0] != "apply-latest" && args[0] != "finish-updater-handoff") {
		fmt.Fprintln(stderr, "usage: fleet update adopt|apply-latest|finish-updater-handoff [guarded flags]")
		return exitUsage
	}
	operation := args[0]
	flags := flag.NewFlagSet("fleet update "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	layout := hostpaths.Default()
	root := flags.String("root", layout.Root, "immutable installation root")
	stateDir := flags.String("state-dir", layout.StateDir, "fleet state directory")
	launchAgentsDir := flags.String("launch-agents-dir", layout.UnitsDir, "per-user service definition directory")
	repository := flags.String("repo", "vitalyiegorov/tart-runner-fleet", "GitHub release repository")
	mode := flags.String("mode", "authority", "preserved controller mode")
	configPath := flags.String("config", layout.ConfigPath, "persisted fleet configuration")
	endpoint := flags.String("endpoint", layout.Endpoint(), "local readiness endpoint")
	domain := flags.String("domain", layout.ServiceDomain, "per-user service supervisor domain")
	confirm := flags.String("confirm", "", "exact mutation confirmation")
	releaseDir := flags.String("release-dir", "", "current immutable release directory (adopt only)")
	interval := flags.Duration("interval", 5*time.Minute, "production release poll interval")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return exitUsage
	}
	wantConfirm := "automatic-release-update"
	switch operation {
	case "adopt":
		wantConfirm = "adopt-current-generation"
	case "finish-updater-handoff":
		wantConfirm = "automatic-updater-handoff"
	}
	if *confirm != wantConfirm {
		fmt.Fprintf(stderr, "unsafe update: require --confirm %s\n", wantConfirm)
		return exitUnsafe
	}
	host, err := autoupdate.NewLocalHost(autoupdate.LocalHostConfig{RootDir: *root, StateDir: *stateDir,
		LaunchAgentsDir: *launchAgentsDir, Domain: *domain, Repository: *repository, UpdateInterval: *interval,
		ReadyAttempts: updateReadyAttempts, ReadyDelay: updateReadyDelay}, deps.command)
	if err != nil {
		fmt.Fprintf(stderr, "configure updater: %v\n", err)
		return exitFailure
	}
	if operation == "adopt" {
		if *releaseDir == "" {
			fmt.Fprintln(stderr, "adopt requires --release-dir")
			return exitUsage
		}
		candidate := autoupdate.Generation{Version: filepath.Base(filepath.Clean(*releaseDir)), Mode: *mode,
			ReleaseDir: *releaseDir, ConfigPath: *configPath, Endpoint: *endpoint}
		if err := host.Adopt(ctx, candidate); err != nil {
			fmt.Fprintf(stderr, "adopt generation: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "automatic production updates enabled for %s\n", candidate.Version)
		return exitSuccess
	}
	if operation == "finish-updater-handoff" {
		if *releaseDir == "" {
			fmt.Fprintln(stderr, "finish-updater-handoff requires --release-dir")
			return exitUsage
		}
		candidate := autoupdate.Generation{Version: filepath.Base(filepath.Clean(*releaseDir)), Mode: *mode,
			ReleaseDir: *releaseDir, ConfigPath: *configPath, Endpoint: *endpoint}
		if err := host.FinishUpdaterHandoff(ctx, candidate); err != nil {
			fmt.Fprintf(stderr, "finish automatic updater handoff: %v\n", err)
			return exitFailure
		}
		fmt.Fprintf(stdout, "automatic updater handoff complete: %s\n", candidate.Version)
		return exitSuccess
	}
	if *releaseDir != "" {
		fmt.Fprintln(stderr, "--release-dir is valid only with adopt")
		return exitUsage
	}
	release, err := autoupdate.LatestProductionRelease(ctx, *root, *repository, deps.command)
	if err != nil {
		fmt.Fprintf(stderr, "resolve production release: %v\n", err)
		return exitFailure
	}
	candidate := autoupdate.Generation{Version: release.Version, Mode: *mode, ReleaseDir: release.Dir,
		ConfigPath: *configPath, Endpoint: *endpoint}
	if err := (autoupdate.Controller{Host: host}).Apply(ctx, candidate); err != nil {
		fmt.Fprintf(stderr, "apply production release: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "production generation is current: %s\n", release.Version)
	return exitSuccess
}

func runScaleSets(ctx context.Context, args []string, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) == 0 || args[0] != "provision" {
		fmt.Fprintln(stderr, "usage: fleet scale-sets provision --config path [--output table|json] [--apply --write --confirm provision-scale-sets --reason text] [--reconcile-drift]")
		return exitUsage
	}
	flags := flag.NewFlagSet("fleet scale-sets provision", flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "fleet configuration path")
	output := flags.String("output", "table", "output format: table or json")
	apply := flags.Bool("apply", false, "create missing scale sets after a complete drift-free plan")
	write := flags.Bool("write", false, "atomically persist returned IDs to the configuration")
	confirm := flags.String("confirm", "", "exact mutation confirmation")
	reason := flags.String("reason", "", "operator reason recorded outside secret material")
	reconcileDrift := flags.Bool("reconcile-drift", false,
		"repair an existing scale set whose GitHub object no longer matches configuration")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *path == "" || (*output != "table" && *output != "json") {
		return exitUsage
	}
	if *apply && (!*write || *confirm != "provision-scale-sets" || strings.TrimSpace(*reason) == "") {
		fmt.Fprintln(stderr, "unsafe apply: require --write --confirm provision-scale-sets and a non-empty --reason")
		return exitUnsafe
	}
	if !*apply && (*write || *confirm != "" || *reason != "") {
		fmt.Fprintln(stderr, "mutation flags require --apply")
		return exitUsage
	}
	file, err := deps.openConfig(*path)
	if err != nil {
		fmt.Fprintf(stderr, "open config: %v\n", err)
		return exitFailure
	}
	cfg, decodeErr := config.Decode(file)
	closeErr := file.Close()
	if decodeErr != nil {
		fmt.Fprintf(stderr, "invalid config: %v\n", decodeErr)
		return exitFailure
	}
	if closeErr != nil {
		fmt.Fprintf(stderr, "close config: %v\n", closeErr)
		return exitFailure
	}
	// Repairing an existing GitHub object is a strictly larger authority than
	// creating a missing one, so it needs its own opt-in rather than riding along
	// on the provision confirmation.
	open := deps.openProvision
	if *reconcileDrift {
		open = deps.openReconcilingProvision
	}
	result, err := provision.Run(ctx, provision.Request{Config: cfg, Apply: *apply, ReconcileDrift: *reconcileDrift,
		LoadKey: deps.loadPrivateKey, Open: open, Version: deps.buildVersion()})
	if err != nil {
		fmt.Fprintf(stderr, "provision scale sets: %v\n", err)
		if errors.Is(err, operations.ErrConflict) || errors.Is(err, operations.ErrUncertain) {
			return exitUnsafe
		}
		return exitFailure
	}
	if *apply {
		if err := deps.writeConfig(*path, result.Config); err != nil {
			fmt.Fprintf(stderr, "persist config: %v\n", err)
			return exitFailure
		}
	}
	if *output == "json" {
		if err := writeJSON(stdout, result.Changes); err != nil {
			return exitFailure
		}
	} else {
		for _, change := range result.Changes {
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\t%s\n", change.Scope, change.Profile, change.Name, change.ID, change.Action)
		}
	}
	return exitSuccess
}

func atomicWriteConfig(path string, cfg config.Config) error {
	return atomicWriteConfigWith(path, cfg, atomicConfigOps{
		createTemp: func(directory, pattern string) (atomicConfigFile, error) { return os.CreateTemp(directory, pattern) },
		remove:     os.Remove,
		rename:     os.Rename,
	})
}

type atomicConfigFile interface {
	io.Writer
	Name() string
	Chmod(os.FileMode) error
	Sync() error
	Close() error
}

type atomicConfigOps struct {
	createTemp func(string, string) (atomicConfigFile, error)
	remove     func(string) error
	rename     func(string, string) error
}

func atomicWriteConfigWith(path string, cfg config.Config, ops atomicConfigOps) error {
	directory := filepath.Dir(path)
	temporary, err := ops.createTemp(directory, ".fleet-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = ops.remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := config.Encode(temporary, cfg); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := ops.rename(temporaryPath, path); err != nil {
		return err
	}
	remove = false
	return nil
}

func splitLeadingConnectionArgs(args []string) (connectionArgs, remaining []string) {
	for len(args) > 0 {
		switch {
		case args[0] == "--endpoint" || args[0] == "--timeout":
			connectionArgs = append(connectionArgs, args[0])
			args = args[1:]
			if len(args) == 0 {
				return connectionArgs, nil
			}
			connectionArgs = append(connectionArgs, args[0])
			args = args[1:]
		case strings.HasPrefix(args[0], "--endpoint=") || strings.HasPrefix(args[0], "--timeout="):
			connectionArgs = append(connectionArgs, args[0])
			args = args[1:]
		default:
			return connectionArgs, args
		}
	}
	return connectionArgs, args
}

type remoteOptions struct {
	endpoint     string
	timeout      time.Duration
	output       string
	requireReady bool
}

func parseRemote(command string, args []string, stderr io.Writer) (remoteOptions, int) {
	flags := flag.NewFlagSet("fleet "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return parseRemoteInto(flags, args, stderr)
}

// parseRemoteInto adds the shared connection and output flags to a caller's flag
// set, so a guarded command accepts exactly the same --endpoint/--timeout/--output
// grammar as an observation command without restating it.
func parseRemoteInto(flags *flag.FlagSet, args []string, stderr io.Writer) (remoteOptions, int) {
	opts := remoteOptions{}
	flags.StringVar(&opts.endpoint, "endpoint", adminapi.DefaultEndpoint(), "local unix:// or loopback http:// endpoint")
	flags.DurationVar(&opts.timeout, "timeout", 5*time.Second, "request timeout (maximum 30s)")
	flags.StringVar(&opts.output, "output", "table", "output format: table or json")
	flags.StringVar(&opts.output, "o", "table", "output format: table or json")
	flags.BoolVar(&opts.requireReady, "require-ready", false, "exit 5 unless the controller is ready")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(stderr, "unexpected positional arguments")
		}
		return remoteOptions{}, exitUsage
	}
	if opts.output != "table" && opts.output != "json" {
		fmt.Fprintf(stderr, "invalid output %q: use table or json\n", opts.output)
		return remoteOptions{}, exitUsage
	}
	if opts.timeout <= 0 || opts.timeout > 30*time.Second {
		fmt.Fprintln(stderr, "invalid timeout: use a duration between 0 and 30s")
		return remoteOptions{}, exitUsage
	}
	return opts, exitSuccess
}

func runRemote(ctx context.Context, command string, args []string, stdout, stderr io.Writer, deps dependencies) int {
	opts, code := parseRemote(command, args, stderr)
	if code != exitSuccess {
		return code
	}
	client, err := deps.newClient(opts.endpoint, opts.timeout)
	if err != nil {
		fmt.Fprintf(stderr, "connect: %v\n", err)
		return exitUnavailable
	}
	switch command {
	case "metrics":
		metrics, err := client.Metrics(ctx)
		if err != nil {
			return remoteError(stderr, err)
		}
		fmt.Fprint(stdout, metrics)
		return exitSuccess
	case "health":
		return runHealth(ctx, client, opts.output, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, client, opts.output, stdout, stderr)
	default:
		status, err := client.Status(ctx)
		if err != nil {
			return remoteError(stderr, err)
		}
		if opts.output == "json" {
			if err := writeJSON(stdout, viewFor(command, status)); err != nil {
				return remoteError(stderr, err)
			}
		} else {
			renderCommand(stdout, command, status)
		}
		if opts.requireReady && !status.Data.Ready.OK {
			return exitDegraded
		}
		return exitSuccess
	}
}

func viewFor(command string, status adminapi.StatusEnvelope) any {
	switch command {
	case "queues":
		if len(status.Data.ScopeQueues) > 0 {
			return struct {
				Profiles []adminapi.Queue      `json:"profiles"`
				Scopes   []adminapi.ScopeQueue `json:"scopes"`
			}{Profiles: status.Data.Queues, Scopes: status.Data.ScopeQueues}
		}
		return status.Data.Queues
	case "instances":
		return status.Data.Instances
	case "operations":
		return status.Data.Operations
	case "observations":
		return status.Data.Observations
	default:
		return status
	}
}

func runHealth(ctx context.Context, client apiClient, output string, stdout, stderr io.Writer) int {
	live, err := client.Probe(ctx, false)
	if err != nil {
		return remoteError(stderr, err)
	}
	ready, err := client.Probe(ctx, true)
	if err != nil {
		return remoteError(stderr, err)
	}
	result := struct {
		APIVersion string         `json:"apiVersion"`
		Live       adminapi.Check `json:"live"`
		Ready      adminapi.Check `json:"ready"`
	}{adminapi.APIVersion, live, ready}
	if output == "json" {
		_ = writeJSON(stdout, result)
	} else {
		renderCheck(stdout, "live", live)
		renderCheck(stdout, "ready", ready)
	}
	if !live.OK || !ready.OK {
		return exitDegraded
	}
	return exitSuccess
}

func runDoctor(ctx context.Context, client apiClient, output string, stdout, stderr io.Writer) int {
	status, err := client.Status(ctx)
	if err != nil {
		return remoteError(stderr, err)
	}
	metrics, err := client.Metrics(ctx)
	if err != nil {
		return remoteError(stderr, err)
	}
	queueSLO := status.Data.EffectiveQueueSLO()
	checks := []doctorCheck{
		{Name: "admin API", OK: status.APIVersion == adminapi.APIVersion, Detail: status.APIVersion},
		{Name: "daemon live", OK: status.Data.Live.OK, Detail: joinReasons(status.Data.Live)},
		{Name: "scheduler ready", OK: status.Data.Ready.OK, Detail: joinReasons(status.Data.Ready)},
		{Name: "queue SLO", OK: queueSLO.OK, Detail: joinReasons(queueSLO)},
		{Name: "metrics", OK: metrics != "", Detail: "bounded endpoint responds"},
	}
	if output == "json" {
		_ = writeJSON(stdout, struct {
			APIVersion string        `json:"apiVersion"`
			Checks     []doctorCheck `json:"checks"`
		}{adminapi.APIVersion, checks})
	} else {
		renderDoctor(stdout, checks)
	}
	for _, check := range checks {
		if !check.OK {
			return exitDegraded
		}
	}
	return exitSuccess
}

func runVersion(args []string, stdout, stderr io.Writer, version string) int {
	flags := flag.NewFlagSet("fleet version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "table", "output format: table or json")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || (*output != "table" && *output != "json") {
		if *output != "table" && *output != "json" {
			fmt.Fprintln(stderr, "invalid output: use table or json")
		}
		return exitUsage
	}
	if *output == "json" {
		_ = writeJSON(stdout, struct {
			Version    string `json:"version"`
			APIVersion string `json:"apiVersion"`
		}{version, adminapi.APIVersion})
	} else {
		fmt.Fprintln(stdout, version)
	}
	return exitSuccess
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "validate" {
		fmt.Fprintln(stderr, "usage: fleet config validate [--mode observe|shadow|canary|authority] [--output table|json] <path>")
		return exitUsage
	}
	flags := flag.NewFlagSet("fleet config validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", string(reconcile.Observe), "controller mode: observe, shadow, canary, or authority")
	output := flags.String("output", "table", "output format: table or json")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 1 || (*output != "table" && *output != "json") {
		return exitUsage
	}
	controllerMode := reconcile.Mode(*mode)
	if !controllerMode.Valid() {
		fmt.Fprintln(stderr, "invalid mode: use observe, shadow, canary, or authority")
		return exitUsage
	}
	path := flags.Arg(0)
	// #nosec G304 -- the operator explicitly selects the configuration file to validate.
	file, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "open config: %v\n", err)
		return exitFailure
	}
	defer file.Close()
	cfg, err := config.Decode(file)
	if err != nil {
		fmt.Fprintf(stderr, "invalid config: %v\n", err)
		return exitFailure
	}
	if controllerMode != reconcile.Observe {
		if err := cfg.ValidateAuthority(); err != nil {
			fmt.Fprintf(stderr, "invalid config: %v\n", err)
			return exitFailure
		}
	}
	// Exercise the same binding construction fleetd runs at startup so a config
	// that passes decode/validate can never crash-loop the daemon on runtime
	// invariants (unknown profile, non-positive durable ID, identity collision).
	if err := app.ValidateBindings(cfg); err != nil {
		fmt.Fprintf(stderr, "invalid config: %v\n", err)
		return exitFailure
	}
	if *output == "json" {
		_ = writeJSON(stdout, struct {
			Valid bool   `json:"valid"`
			Path  string `json:"path"`
		}{true, path})
	} else {
		fmt.Fprintf(stdout, "configuration is valid: %s\n", path)
	}
	return exitSuccess
}

func remoteError(stderr io.Writer, err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(stderr, "request canceled")
	} else {
		fmt.Fprintf(stderr, "fleet unavailable: %v\n", err)
	}
	return exitUnavailable
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeHelp(output io.Writer) {
	fmt.Fprint(output, `fleet — safe operator interface for Tart Runner Fleet

READ-ONLY COMMANDS (observe/shadow safe)
  fleet status [--output table|json] [--require-ready]
  fleet queues|instances|operations|observations [--output table|json]
  fleet health|doctor [--output table|json]
  fleet metrics
  fleet config validate [--mode observe|shadow|canary|authority] <path>
  fleet version | api-version

GUARDED BOOTSTRAP
  fleet scale-sets provision --config path
  fleet scale-sets provision --config path --apply --write \
    --confirm provision-scale-sets --reason "operator reason"

GUARDED DEAD-LETTER DISCHARGE
  fleet operations discharge --operation op-ID --instance trf-ID \
    --confirm discharge-dead-letter --reason "operator reason"
  fleet operations discharge --operation op-ID --instance trf-ID --reap-instance \
    --confirm discharge-dead-letter --reason "operator reason"
  --reap-instance also retires the instance row and deletes its stopped VM. It
  is refused while the VM is running. See docs/OPERATIONS.md.

GUARDED RELEASE UPDATES
  fleet update adopt --release-dir /absolute/release --mode authority \
    --confirm adopt-current-generation
  fleet update apply-latest --mode authority \
    --confirm automatic-release-update

CONNECTION
  fleet [--endpoint unix:///path/to/fleetd.sock] <remote-command>
  fleet <remote-command> [--endpoint unix:///path/to/fleetd.sock]
  --timeout 5s may appear in either position; the default endpoint uses the
  private tart-runner-fleet state directory.

EXIT CODES
  0 success  1 failure  2 usage  3 not-found  4 unavailable  5 degraded  6 unsafe

Mutations are refused unless the daemon runs in authority mode.
`)
}
