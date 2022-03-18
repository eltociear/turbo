package run

import (
	"bufio"
	gocontext "context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/vercel/turborepo/cli/internal/analytics"
	"github.com/vercel/turborepo/cli/internal/api"
	"github.com/vercel/turborepo/cli/internal/cache"
	"github.com/vercel/turborepo/cli/internal/config"
	"github.com/vercel/turborepo/cli/internal/context"
	"github.com/vercel/turborepo/cli/internal/core"
	"github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/globby"
	"github.com/vercel/turborepo/cli/internal/logstreamer"
	"github.com/vercel/turborepo/cli/internal/process"
	"github.com/vercel/turborepo/cli/internal/scm"
	"github.com/vercel/turborepo/cli/internal/scope"
	"github.com/vercel/turborepo/cli/internal/ui"
	"github.com/vercel/turborepo/cli/internal/util"
	"github.com/vercel/turborepo/cli/internal/util/browser"

	"github.com/pyr-sh/dag"

	"github.com/fatih/color"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/cli"
	"github.com/pkg/errors"
)

const TOPOLOGICAL_PIPELINE_DELIMITER = "^"
const ENV_PIPELINE_DELIMITER = "$"

// RunCommand is a Command implementation that tells Turbo to run a task
type RunCommand struct {
	Config    *config.Config
	Ui        *cli.ColoredUi
	Processes *process.Manager
}

// completeGraph represents the common state inferred from the filesystem and pipeline.
// It is not intended to include information specific to a particular run.
type completeGraph struct {
	TopologicalGraph dag.AcyclicGraph
	Pipeline         map[string]fs.Pipeline
	SCC              [][]dag.Vertex
	PackageInfos     map[interface{}]*fs.PackageJSON
	GlobalHash       string
	RootNode         string
}

// runSpec contains the run-specific configuration elements that come from a particular
// invocation of turbo.
type runSpec struct {
	Targets      []string
	FilteredPkgs util.Set
	Opts         *RunOptions
}

type LogsMode string

const (
	FullLogs LogsMode = "full"
	HashLogs LogsMode = "hash"
	NoLogs   LogsMode = "none"
)

func (rs *runSpec) ArgsForTask(task string) []string {
	passThroughArgs := make([]string, 0, len(rs.Opts.passThroughArgs))
	for _, target := range rs.Targets {
		if target == task {
			passThroughArgs = append(passThroughArgs, rs.Opts.passThroughArgs...)
		}
	}
	return passThroughArgs
}

// Synopsis of run command
func (c *RunCommand) Synopsis() string {
	return "Run a task"
}

// Help returns information about the `run` command
func (c *RunCommand) Help() string {
	helpText := strings.TrimSpace(`
Usage: turbo run <task> [options] ...

    Run tasks across projects in your monorepo.

    By default, turbo executes tasks in topological order (i.e.
    dependencies first) and then caches the results. Re-running commands for
    tasks already in the cache will skip re-execution and immediately move
    artifacts from the cache into the correct output folders (as if the task
    occurred again).

Options:
  --help                 Show this message.
  --scope                Specify package(s) to act as entry points for task
                         execution. Supports globs.
  --cache-dir            Specify local filesystem cache directory.
                         (default "./node_modules/.cache/turbo")
  --concurrency          Limit the concurrency of task execution. Use 1 for
                         serial (i.e. one-at-a-time) execution. (default 10)
  --continue             Continue execution even if a task exits with an error
                         or non-zero exit code. The default behavior is to bail
                         immediately. (default false)
  --force                Ignore the existing cache (to force execution).
                         (default false)
  --graph                Generate a Dot graph of the task execution.
  --global-deps          Specify glob of global filesystem dependencies to
                         be hashed. Useful for .env and files in the root
                         directory. Can be specified multiple times.
  --since                Limit/Set scope to changed packages since a
                         mergebase. This uses the git diff ${target_branch}...
                         mechanism to identify which packages have changed.
  --team                 The slug of the turborepo.com team.
  --token                A turborepo.com personal access token.
  --ignore               Files to ignore when calculating changed files
                         (i.e. --since). Supports globs.
  --profile              File to write turbo's performance profile output into.
                         You can load the file up in chrome://tracing to see
                         which parts of your build were slow.
  --parallel             Execute all tasks in parallel. (default false)
  --include-dependencies Include the dependencies of tasks in execution.
                         (default false)
  --no-deps              Exclude dependent task consumers from execution.
                         (default false)
  --no-cache             Avoid saving task results to the cache. Useful for
                         development/watch tasks. (default false)
  --output-logs          Set type of process output logging. Use full to show
                         all output. Use hash-only to show only turbo-computed
                         task hashes. Use new-only to show only new output with
                         only hashes for cached tasks. Use none to hide process
                         output. (default full)
  --dry/--dry-run[=json] List the packages in scope and the tasks that would be run,
                         but don't actually run them. Passing --dry=json or
                         --dry-run=json will render the output in JSON format.
`)
	return strings.TrimSpace(helpText)
}

// Run executes tasks in the monorepo
func (c *RunCommand) Run(args []string) int {
	startAt := time.Now()
	log.SetFlags(0)
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.Usage = func() { c.Config.Logger.Info(c.Help()) }
	if err := flags.Parse(args); err != nil {
		return 1
	}

	runOptions, err := parseRunArgs(args, c.Ui)
	if err != nil {
		c.logError(c.Config.Logger, "", err)
		return 1
	}

	c.Config.Cache.Dir = runOptions.cacheFolder

	ctx, err := context.New(context.WithGraph(runOptions.cwd, c.Config))
	if err != nil {
		c.logError(c.Config.Logger, "", err)
		return 1
	}
	targets, err := getTargetsFromArguments(args, ctx.TurboConfig)
	if err != nil {
		c.logError(c.Config.Logger, "", fmt.Errorf("failed to resolve targets: %w", err))
		return 1
	}

	scmInstance, err := scm.FromInRepo(runOptions.cwd)
	if err != nil {
		if errors.Is(err, scm.ErrFallback) {
			c.logWarning(c.Config.Logger, "", err)
		} else {
			c.logError(c.Config.Logger, "", fmt.Errorf("failed to create SCM: %w", err))
			return 1
		}
	}
	filteredPkgs, err := scope.ResolvePackages(runOptions.ScopeOpts(), scmInstance, ctx, c.Ui, c.Config.Logger)
	if err != nil {
		c.logError(c.Config.Logger, "", fmt.Errorf("failed resolve packages to run %v", err))
	}
	c.Config.Logger.Debug("global hash", "value", ctx.GlobalHash)
	c.Config.Logger.Debug("local cache folder", "path", runOptions.cacheFolder)
	fs.EnsureDir(runOptions.cacheFolder)

	// TODO: consolidate some of these arguments
	g := &completeGraph{
		TopologicalGraph: ctx.TopologicalGraph,
		Pipeline:         ctx.TurboConfig.Pipeline,
		SCC:              ctx.SCC,
		PackageInfos:     ctx.PackageInfos,
		GlobalHash:       ctx.GlobalHash,
		RootNode:         ctx.RootNode,
	}
	rs := &runSpec{
		Targets:      targets,
		FilteredPkgs: filteredPkgs,
		Opts:         runOptions,
	}
	backend := ctx.Backend
	return c.runOperation(g, rs, backend, startAt)
}

func (c *RunCommand) runOperation(g *completeGraph, rs *runSpec, backend *api.LanguageBackend, startAt time.Time) int {
	var topoVisit []interface{}
	for _, node := range g.SCC {
		v := node[0]
		if v == g.RootNode {
			continue
		}
		topoVisit = append(topoVisit, v)
		pack := g.PackageInfos[v]

		ancestralHashes := make([]string, 0, len(pack.InternalDeps))
		if len(pack.InternalDeps) > 0 {
			for _, ancestor := range pack.InternalDeps {
				if h, ok := g.PackageInfos[ancestor]; ok {
					ancestralHashes = append(ancestralHashes, h.Hash)
				}
			}
			sort.Strings(ancestralHashes)
		}
		var hashable = struct {
			hashOfFiles      string
			ancestralHashes  []string
			externalDepsHash string
			globalHash       string
		}{hashOfFiles: pack.FilesHash, ancestralHashes: ancestralHashes, externalDepsHash: pack.ExternalDepsHash, globalHash: g.GlobalHash}

		var err error
		pack.Hash, err = fs.HashObject(hashable)
		if err != nil {
			c.logError(c.Config.Logger, "", fmt.Errorf("[ERROR] %v: error computing combined hash: %v", pack.Name, err))
			return 1
		}
		c.Config.Logger.Debug(fmt.Sprintf("%v: package ancestralHash", pack.Name), "hash", ancestralHashes)
		c.Config.Logger.Debug(fmt.Sprintf("%v: package hash", pack.Name), "hash", pack.Hash)
	}

	c.Config.Logger.Debug("topological sort order", "value", topoVisit)

	vertexSet := make(util.Set)
	for _, v := range g.TopologicalGraph.Vertices() {
		vertexSet.Add(v)
	}
	// We remove nodes that aren't in the final filter set
	for _, toRemove := range vertexSet.Difference(rs.FilteredPkgs) {
		if toRemove != g.RootNode {
			g.TopologicalGraph.Remove(toRemove)
		}
	}

	// If we are running in parallel, then we remove all the edges in the graph
	// except for the root
	if rs.Opts.parallel {
		for _, edge := range g.TopologicalGraph.Edges() {
			if edge.Target() != g.RootNode {
				g.TopologicalGraph.RemoveEdge(edge)
			}
		}
	}

	engine, err := buildTaskGraph(&g.TopologicalGraph, g.Pipeline, rs)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error preparing engine: %s", err))
		return 1
	}
	exitCode := 0
	if rs.Opts.dotGraph != "" {
		err := c.generateDotGraph(engine.TaskGraph, filepath.Join(rs.Opts.cwd, rs.Opts.dotGraph))
		if err != nil {
			c.logError(c.Config.Logger, "", err)
			return 1
		}
	} else if rs.Opts.dryRun {
		tasksRun, err := c.executeDryRun(engine, g, rs, c.Config.Logger)
		if err != nil {
			c.logError(c.Config.Logger, "", err)
			return 1
		}
		packagesInScope := rs.FilteredPkgs.UnsafeListOfStrings()
		sort.Strings(packagesInScope)
		if rs.Opts.dryRunJson {
			dryRun := &struct {
				Packages []string     `json:"packages"`
				Tasks    []hashedTask `json:"tasks"`
			}{
				Packages: packagesInScope,
				Tasks:    tasksRun,
			}
			bytes, err := json.MarshalIndent(dryRun, "", "  ")
			if err != nil {
				c.logError(c.Config.Logger, "", errors.Wrap(err, "failed to render to JSON"))
				return 1
			}
			c.Ui.Output(string(bytes))
		} else {
			c.Ui.Output("")
			c.Ui.Info(util.Sprintf("${CYAN}${BOLD}Packages in Scope${RESET}"))
			p := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
			fmt.Fprintln(p, "Name\tPath\t")
			for _, pkg := range packagesInScope {
				fmt.Fprintln(p, fmt.Sprintf("%s\t%s\t", pkg, g.PackageInfos[pkg].Dir))
			}
			p.Flush()

			c.Ui.Output("")
			c.Ui.Info(util.Sprintf("${CYAN}${BOLD}Tasks to Run${RESET}"))

			for _, task := range tasksRun {
				c.Ui.Info(util.Sprintf("${BOLD}%s${RESET}", task.TaskID))
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', 0)
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Task\t=\t%s\t${RESET}", task.Task))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Package\t=\t%s\t${RESET}", task.Package))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Hash\t=\t%s\t${RESET}", task.Hash))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Directory\t=\t%s\t${RESET}", task.Dir))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Command\t=\t%s\t${RESET}", task.Command))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Outputs\t=\t%s\t${RESET}", strings.Join(task.Outputs, ", ")))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Log File\t=\t%s\t${RESET}", task.LogFile))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Dependencies\t=\t%s\t${RESET}", strings.Join(task.Dependencies, ", ")))
				fmt.Fprintln(w, util.Sprintf("  ${GREY}Dependendents\t=\t%s\t${RESET}", strings.Join(task.Dependents, ", ")))
				w.Flush()
			}

		}
	} else {
		packagesInScope := rs.FilteredPkgs.UnsafeListOfStrings()
		sort.Strings(packagesInScope)
		c.Ui.Output(fmt.Sprintf(ui.Dim("• Packages in scope: %v"), strings.Join(packagesInScope, ", ")))
		if rs.Opts.stream {
			c.Ui.Output(fmt.Sprintf("%s %s %s", ui.Dim("• Running"), ui.Dim(ui.Bold(strings.Join(rs.Targets, ", "))), ui.Dim(fmt.Sprintf("in %v packages", rs.FilteredPkgs.Len()))))
		}
		exitCode = c.executeTasks(g, rs, engine, backend, startAt)
	}

	return exitCode
}

func buildTaskGraph(topoGraph *dag.AcyclicGraph, pipeline map[string]fs.Pipeline, rs *runSpec) (*core.Scheduler, error) {
	engine := core.NewScheduler(topoGraph)
	for taskName, value := range pipeline {
		topoDeps := make(util.Set)
		deps := make(util.Set)
		if util.IsPackageTask(taskName) {
			for _, from := range value.DependsOn {
				if strings.HasPrefix(from, ENV_PIPELINE_DELIMITER) {
					continue
				}
				if util.IsPackageTask(from) {
					engine.AddDep(from, taskName)
					continue
				} else if strings.Contains(from, TOPOLOGICAL_PIPELINE_DELIMITER) {
					topoDeps.Add(from[1:])
				} else {
					deps.Add(from)
				}
			}
			_, id := util.GetPackageTaskFromId(taskName)
			taskName = id
		} else {
			for _, from := range value.DependsOn {
				if strings.HasPrefix(from, ENV_PIPELINE_DELIMITER) {
					continue
				}
				if strings.Contains(from, TOPOLOGICAL_PIPELINE_DELIMITER) {
					topoDeps.Add(from[1:])
				} else {
					deps.Add(from)
				}
			}
		}

		engine.AddTask(&core.Task{
			Name:     taskName,
			TopoDeps: topoDeps,
			Deps:     deps,
		})
	}

	if err := engine.Prepare(&core.SchedulerExecutionOptions{
		Packages:  rs.FilteredPkgs.UnsafeListOfStrings(),
		TaskNames: rs.Targets,
		TasksOnly: rs.Opts.only,
	}); err != nil {
		return nil, err
	}
	return engine, nil
}

// RunOptions holds the current run operations configuration

type RunOptions struct {
	// Whether to include dependent impacted consumers in execution (defaults to true)
	includeDependents bool
	// Whether to include includeDependencies (pkg.dependencies) in execution (defaults to false)
	includeDependencies bool
	// List of globs of file paths to ignore from execution scope calculation
	ignore []string
	// Whether to stream log outputs
	stream bool
	// Show a dot graph
	dotGraph string
	// List of globs to global files whose contents will be included in the global hash calculation
	globalDeps []string
	// Filtered list of package entrypoints
	scope []string
	// Force execution to be serially one-at-a-time
	concurrency int
	// Whether to execute in parallel (defaults to false)
	parallel bool
	// Git diff used to calculate changed packages
	since string
	// Current working directory
	cwd string
	// Whether to emit a perf profile
	profile string
	// Force task execution
	forceExecution bool
	// Cache results
	cache bool
	// Cache folder
	cacheFolder string
	// Immediately exit on task failure
	bail            bool
	passThroughArgs []string
	// Restrict execution to only the listed task names. Default false
	only bool
	// Task logs output modes (cached and not cached tasks):
	// full - show all,
	// hash - only show task hash,
	// none - show nothing
	cacheHitLogsMode  LogsMode
	cacheMissLogsMode LogsMode
	dryRun            bool
	dryRunJson        bool
}

func (ro *RunOptions) ScopeOpts() *scope.Opts {
	return &scope.Opts{
		IncludeDependencies: ro.includeDependencies,
		IncludeDependents:   ro.includeDependents,
		Patterns:            ro.scope,
		Since:               ro.since,
		Cwd:                 ro.cwd,
		IgnorePatterns:      ro.ignore,
		GlobalDepPatterns:   ro.globalDeps,
	}
}

func getDefaultRunOptions() *RunOptions {
	return &RunOptions{
		bail:                true,
		includeDependents:   true,
		parallel:            false,
		concurrency:         10,
		dotGraph:            "",
		includeDependencies: false,
		cache:               true,
		profile:             "", // empty string does no tracing
		forceExecution:      false,
		stream:              true,
		only:                false,
		cacheHitLogsMode:    FullLogs,
		cacheMissLogsMode:   FullLogs,
	}
}

func parseRunArgs(args []string, output cli.Ui) (*RunOptions, error) {
	var runOptions = getDefaultRunOptions()

	if len(args) == 0 {
		return nil, errors.Errorf("At least one task must be specified.")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("invalid working directory: %w", err)
	}
	runOptions.cwd = cwd

	unresolvedCacheFolder := filepath.FromSlash("./node_modules/.cache/turbo")

	// --scope and --since implies --include-dependencies for backwards compatibility
	// When we switch to cobra we will need to track if it's been set manually. Currently
	// it's only possible to set to true, but in the future a user could theoretically set
	// it to false and override the default behavior.
	includDepsSet := false
	for argIndex, arg := range args {
		if arg == "--" {
			runOptions.passThroughArgs = args[argIndex+1:]
			break
		} else if strings.HasPrefix(arg, "--") {
			switch {
			case strings.HasPrefix(arg, "--since="):
				if len(arg[len("--since="):]) > 0 {
					runOptions.since = arg[len("--since="):]
				}
			case strings.HasPrefix(arg, "--scope="):
				if len(arg[len("--scope="):]) > 0 {
					runOptions.scope = append(runOptions.scope, arg[len("--scope="):])
				}
			case strings.HasPrefix(arg, "--ignore="):
				if len(arg[len("--ignore="):]) > 0 {
					runOptions.ignore = append(runOptions.ignore, arg[len("--ignore="):])
				}
			case strings.HasPrefix(arg, "--global-deps="):
				if len(arg[len("--global-deps="):]) > 0 {
					runOptions.globalDeps = append(runOptions.globalDeps, arg[len("--global-deps="):])
				}
			case strings.HasPrefix(arg, "--cwd="):
				if len(arg[len("--cwd="):]) > 0 {
					runOptions.cwd = arg[len("--cwd="):]
				}
			case strings.HasPrefix(arg, "--parallel"):
				runOptions.parallel = true
			case strings.HasPrefix(arg, "--profile="): // this one must com before the next
				if len(arg[len("--profile="):]) > 0 {
					runOptions.profile = arg[len("--profile="):]
				}
			case strings.HasPrefix(arg, "--profile"):
				runOptions.profile = fmt.Sprintf("%v-profile.json", time.Now().UnixNano())

			case strings.HasPrefix(arg, "--no-deps"):
				runOptions.includeDependents = false
			case strings.HasPrefix(arg, "--no-cache"):
				runOptions.cache = false
			case strings.HasPrefix(arg, "--cacheFolder"):
				output.Warn("[WARNING] The --cacheFolder flag has been deprecated and will be removed in future versions of turbo. Please use `--cache-dir` instead")
				unresolvedCacheFolder = arg[len("--cacheFolder="):]
			case strings.HasPrefix(arg, "--cache-dir"):
				unresolvedCacheFolder = arg[len("--cache-dir="):]
			case strings.HasPrefix(arg, "--continue"):
				runOptions.bail = false
			case strings.HasPrefix(arg, "--force"):
				runOptions.forceExecution = true
			case strings.HasPrefix(arg, "--stream"):
				runOptions.stream = true

			case strings.HasPrefix(arg, "--graph="): // this one must com before the next
				if len(arg[len("--graph="):]) > 0 {
					runOptions.dotGraph = arg[len("--graph="):]
				}
			case strings.HasPrefix(arg, "--graph"):
				runOptions.dotGraph = fmt.Sprintf("graph-%v.jpg", time.Now().UnixNano())
			case strings.HasPrefix(arg, "--serial"):
				output.Warn("[WARNING] The --serial flag has been deprecated and will be removed in future versions of turbo. Please use `--concurrency=1` instead")
				runOptions.concurrency = 1
			case strings.HasPrefix(arg, "--concurrency"):
				if i, err := strconv.Atoi(arg[len("--concurrency="):]); err != nil {
					return nil, fmt.Errorf("invalid value for --concurrency CLI flag. This should be a positive integer greater than or equal to 1: %w", err)
				} else {
					if i >= 1 {
						runOptions.concurrency = i
					} else {
						return nil, fmt.Errorf("invalid value %v for --concurrency CLI flag. This should be a positive integer greater than or equal to 1", i)
					}
				}
			case strings.HasPrefix(arg, "--includeDependencies"):
				output.Warn("[WARNING] The --includeDependencies flag has renamed to --include-dependencies for consistency. Please use `--include-dependencies` instead")
				runOptions.includeDependencies = true
				includDepsSet = true
			case strings.HasPrefix(arg, "--include-dependencies"):
				runOptions.includeDependencies = true
				includDepsSet = true
			case strings.HasPrefix(arg, "--only"):
				runOptions.only = true
			case strings.HasPrefix(arg, "--output-logs="):
				outputLogsMode := arg[len("--output-logs="):]
				switch outputLogsMode {
				case "full":
					runOptions.cacheMissLogsMode = FullLogs
					runOptions.cacheHitLogsMode = FullLogs
				case "none":
					runOptions.cacheMissLogsMode = NoLogs
					runOptions.cacheHitLogsMode = NoLogs
				case "hash-only":
					runOptions.cacheMissLogsMode = HashLogs
					runOptions.cacheHitLogsMode = HashLogs
				case "new-only":
					runOptions.cacheMissLogsMode = FullLogs
					runOptions.cacheHitLogsMode = HashLogs
				default:
					output.Warn(fmt.Sprintf("[WARNING] unknown value %v for --output-logs CLI flag. Falling back to full", outputLogsMode))
				}
			case strings.HasPrefix(arg, "--dry-run"):
				runOptions.dryRun = true
				if strings.HasPrefix(arg, "--dry-run=json") {
					runOptions.dryRunJson = true
				}
			case strings.HasPrefix(arg, "--dry"):
				runOptions.dryRun = true
				if strings.HasPrefix(arg, "--dry=json") {
					runOptions.dryRunJson = true
				}
			case strings.HasPrefix(arg, "--team"):
			case strings.HasPrefix(arg, "--token"):
			case strings.HasPrefix(arg, "--api"):
			case strings.HasPrefix(arg, "--url"):
			case strings.HasPrefix(arg, "--trace"):
			case strings.HasPrefix(arg, "--cpuprofile"):
			case strings.HasPrefix(arg, "--heap"):
			case strings.HasPrefix(arg, "--no-gc"):
			default:
				return nil, errors.New(fmt.Sprintf("unknown flag: %v", arg))
			}
		}
	}
	if len(runOptions.scope) != 0 && runOptions.since != "" && !includDepsSet {
		runOptions.includeDependencies = true
	}

	// Force streaming output in CI/CD non-interactive mode
	if !ui.IsTTY || ui.IsCI {
		runOptions.stream = true
	}

	// We can only set this cache folder after we know actual cwd
	runOptions.cacheFolder = filepath.Join(runOptions.cwd, unresolvedCacheFolder)

	return runOptions, nil
}

// logError logs an error and outputs it to the UI.
func (c *RunCommand) logError(log hclog.Logger, prefix string, err error) {
	log.Error(prefix, "error", err)

	if prefix != "" {
		prefix += ": "
	}

	c.Ui.Error(fmt.Sprintf("%s%s%s", ui.ERROR_PREFIX, prefix, color.RedString(" %v", err)))
}

// logError logs an error and outputs it to the UI.
func (c *RunCommand) logWarning(log hclog.Logger, prefix string, err error) {
	log.Warn(prefix, "warning", err)

	if prefix != "" {
		prefix = " " + prefix + ": "
	}

	c.Ui.Error(fmt.Sprintf("%s%s%s", ui.WARNING_PREFIX, prefix, color.YellowString(" %v", err)))
}

func hasGraphViz() bool {
	err := exec.Command("dot", "-v").Run()
	return err == nil
}

func (c *RunCommand) executeTasks(g *completeGraph, rs *runSpec, engine *core.Scheduler, backend *api.LanguageBackend, startAt time.Time) int {
	goctx := gocontext.Background()
	var analyticsSink analytics.Sink
	if c.Config.IsLoggedIn() {
		analyticsSink = c.Config.ApiClient
	} else {
		analyticsSink = analytics.NullSink
	}
	analyticsClient := analytics.NewClient(goctx, analyticsSink, c.Config.Logger.Named("analytics"))
	defer analyticsClient.CloseWithTimeout(50 * time.Millisecond)
	turboCache := cache.New(c.Config, analyticsClient)
	defer turboCache.Shutdown()
	runState := NewRunState(rs.Opts, startAt)
	runState.Listen(c.Ui, time.Now())
	ec := &execContext{
		colorCache: NewColorCache(),
		runState:   runState,
		rs:         rs,
		ui:         &cli.ConcurrentUi{Ui: c.Ui},
		turboCache: turboCache,
		logger:     c.Config.Logger,
		backend:    backend,
		processes:  c.Processes,
	}

	// run the thing
	errs := engine.Execute(g.getPackageTaskVisitor(ec.exec), core.ExecOpts{
		Parallel:    rs.Opts.parallel,
		Concurrency: rs.Opts.concurrency,
	})

	// Track if we saw any child with a non-zero exit code
	exitCode := 0
	exitCodeErr := &process.ChildExit{}
	for _, err := range errs {
		if errors.As(err, &exitCodeErr) {
			if exitCodeErr.ExitCode > exitCode {
				exitCode = exitCodeErr.ExitCode
			}
		}
		c.Ui.Error(err.Error())
	}

	ec.logReplayWaitGroup.Wait()

	if err := runState.Close(c.Ui, rs.Opts.profile); err != nil {
		c.Ui.Error(fmt.Sprintf("Error with profiler: %s", err.Error()))
		return 1
	}
	return 0
}

type hashedTask struct {
	TaskID       string   `json:"taskId"`
	Task         string   `json:"task"`
	Package      string   `json:"package"`
	Hash         string   `json:"hash"`
	Command      string   `json:"command"`
	Outputs      []string `json:"outputs"`
	LogFile      string   `json:"logFile"`
	Dir          string   `json:"directory"`
	Dependencies []string `json:"dependencies"`
	Dependents   []string `json:"dependents"`
}

func (c *RunCommand) executeDryRun(engine *core.Scheduler, g *completeGraph, rs *runSpec, logger hclog.Logger) ([]hashedTask, error) {
	taskIDs := []hashedTask{}
	errs := engine.Execute(g.getPackageTaskVisitor(func(pt *packageTask) error {
		command, ok := pt.pkg.Scripts[pt.task]
		if !ok {
			logger.Debug("no task in package, skipping")
			logger.Debug("done", "status", "skipped")
			return nil
		}
		passThroughArgs := rs.ArgsForTask(pt.task)
		hash, err := pt.hash(passThroughArgs, logger)
		if err != nil {
			return err
		}
		ancestors, err := engine.TaskGraph.Ancestors(pt.taskID)
		if err != nil {
			return err
		}
		stringAncestors := []string{}
		for _, dep := range ancestors {
			// Don't leak out internal ROOT_NODE_NAME nodes, which are just placeholders
			if !strings.Contains(dep.(string), core.ROOT_NODE_NAME) {
				stringAncestors = append(stringAncestors, dep.(string))
			}
		}
		descendents, err := engine.TaskGraph.Descendents(pt.taskID)
		if err != nil {
			return err
		}
		stringDescendents := []string{}
		for _, dep := range descendents {
			// Don't leak out internal ROOT_NODE_NAME nodes, which are just placeholders
			if !strings.Contains(dep.(string), core.ROOT_NODE_NAME) {
				stringDescendents = append(stringDescendents, dep.(string))
			}
		}
		sort.Strings(stringDescendents)

		taskIDs = append(taskIDs, hashedTask{
			TaskID:       pt.taskID,
			Task:         pt.task,
			Package:      pt.packageName,
			Hash:         hash,
			Command:      command,
			Dir:          pt.pkg.Dir,
			Outputs:      pt.ExternalOutputs(),
			LogFile:      pt.RepoRelativeLogFile(),
			Dependencies: stringAncestors,
			Dependents:   stringDescendents,
		})
		return nil
	}), core.ExecOpts{
		Concurrency: 1,
		Parallel:    false,
	})
	if len(errs) > 0 {
		for _, err := range errs {
			c.Ui.Error(err.Error())
		}
		return nil, errors.New("errors occurred during dry-run graph traversal")
	}
	return taskIDs, nil
}

// Replay logs will try to replay logs back to the stdout
func replayLogs(logger hclog.Logger, prefixUi cli.Ui, runOptions *RunOptions, logFileName, hash string, wg *sync.WaitGroup, silent bool, outputLogsMode LogsMode) {
	defer wg.Done()
	logger.Debug("start replaying logs")
	f, err := os.Open(filepath.Join(runOptions.cwd, logFileName))
	if err != nil && !silent {
		prefixUi.Warn(fmt.Sprintf("error reading logs: %v", err))
		logger.Error(fmt.Sprintf("error reading logs: %v", err.Error()))
	}
	defer f.Close()
	if outputLogsMode != NoLogs {
		scan := bufio.NewScanner(f)
		if outputLogsMode == HashLogs {
			//Writing to Stdout only the "cache hit, replaying output" line
			scan.Scan()
			prefixUi.Output(ui.StripAnsi(string(scan.Bytes())))
		} else {
			for scan.Scan() {
				prefixUi.Output(ui.StripAnsi(string(scan.Bytes()))) //Writing to Stdout
			}
		}
	}
	logger.Debug("finish replaying logs")
}

// GetTargetsFromArguments returns a list of targets from the arguments and Turbo config.
// Return targets are always unique sorted alphabetically.
func getTargetsFromArguments(arguments []string, configJson *fs.TurboConfigJSON) ([]string, error) {
	targets := make(util.Set)
	for _, arg := range arguments {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			targets.Add(arg)
			found := false
			for task := range configJson.Pipeline {
				if task == arg {
					found = true
				}
			}
			if !found {
				return nil, fmt.Errorf("task `%v` not found in turbo pipeline in package.json. Are you sure you added it?", arg)
			}
		}
	}
	stringTargets := targets.UnsafeListOfStrings()
	sort.Strings(stringTargets)
	return stringTargets, nil
}

type execContext struct {
	colorCache         *ColorCache
	runState           *RunState
	rs                 *runSpec
	logReplayWaitGroup sync.WaitGroup
	ui                 cli.Ui
	turboCache         cache.Cache
	logger             hclog.Logger
	backend            *api.LanguageBackend
	processes          *process.Manager
}

func (e *execContext) logError(log hclog.Logger, prefix string, err error) {
	e.logger.Error(prefix, "error", err)

	if prefix != "" {
		prefix += ": "
	}

	e.ui.Error(fmt.Sprintf("%s%s%s", ui.ERROR_PREFIX, prefix, color.RedString(" %v", err)))
}

func (e *execContext) exec(pt *packageTask) error {
	cmdTime := time.Now()

	targetLogger := e.logger.Named(fmt.Sprintf("%v:%v", pt.pkg.Name, pt.task))
	targetLogger.Debug("start")

	// bail if the script doesn't exist
	if _, ok := pt.pkg.Scripts[pt.task]; !ok {
		targetLogger.Debug("no task in package, skipping")
		targetLogger.Debug("done", "status", "skipped", "duration", time.Since(cmdTime))
		return nil
	}

	// Setup tracer
	tracer := e.runState.Run(util.GetTaskId(pt.pkg.Name, pt.task))

	// Create a logger
	pref := e.colorCache.PrefixColor(pt.pkg.Name)
	actualPrefix := pref("%s:%s: ", pt.pkg.Name, pt.task)
	targetUi := &cli.PrefixedUi{
		Ui:           e.ui,
		OutputPrefix: actualPrefix,
		InfoPrefix:   actualPrefix,
		ErrorPrefix:  actualPrefix,
		WarnPrefix:   actualPrefix,
	}

	logFileName := filepath.Join(pt.pkg.Dir, ".turbo", fmt.Sprintf("turbo-%v.log", pt.task))
	targetLogger.Debug("log file", "path", filepath.Join(e.rs.Opts.cwd, logFileName))

	passThroughArgs := e.rs.ArgsForTask(pt.task)
	hash, err := pt.hash(passThroughArgs, e.logger)
	e.logger.Debug("task hash", "value", hash)
	if err != nil {
		e.ui.Error(fmt.Sprintf("Hashing error: %v", err))
		// @TODO probably should abort fatally???
	}
	// Cache ---------------------------------------------
	var hit bool
	if !e.rs.Opts.forceExecution {
		hit, _, _, err = e.turboCache.Fetch(e.rs.Opts.cwd, hash, nil)
		if err != nil {
			targetUi.Error(fmt.Sprintf("error fetching from cache: %s", err))
		} else if hit {
			if e.rs.Opts.stream && fs.FileExists(filepath.Join(e.rs.Opts.cwd, logFileName)) {
				e.logReplayWaitGroup.Add(1)
				go replayLogs(targetLogger, e.ui, e.rs.Opts, logFileName, hash, &e.logReplayWaitGroup, false, e.rs.Opts.cacheHitLogsMode)
			}
			targetLogger.Debug("done", "status", "complete", "duration", time.Since(cmdTime))
			tracer(TargetCached, nil)

			return nil
		}
		if e.rs.Opts.stream && e.rs.Opts.cacheHitLogsMode != NoLogs {
			targetUi.Output(fmt.Sprintf("cache miss, executing %s", ui.Dim(hash)))
		}
	} else {
		if e.rs.Opts.stream && e.rs.Opts.cacheHitLogsMode != NoLogs {
			targetUi.Output(fmt.Sprintf("cache bypass, force executing %s", ui.Dim(hash)))
		}
	}

	// Setup command execution
	argsactual := append([]string{"run"}, pt.task)
	argsactual = append(argsactual, passThroughArgs...)
	// @TODO: @jaredpalmer fix this hack to get the package manager's name
	var cmd *exec.Cmd
	if e.backend.Name == "nodejs-berry" {
		cmd = exec.Command("yarn", argsactual...)
	} else {
		cmd = exec.Command(strings.TrimPrefix(e.backend.Name, "nodejs-"), argsactual...)
	}
	cmd.Dir = pt.pkg.Dir
	envs := fmt.Sprintf("TURBO_HASH=%v", hash)
	cmd.Env = append(os.Environ(), envs)

	// Setup stdout/stderr
	// If we are not caching anything, then we don't need to write logs to disk
	// be careful about this conditional given the default of cache = true
	var writer io.Writer
	if !e.rs.Opts.cache || (pt.pipeline.Cache != nil && !*pt.pipeline.Cache) {
		writer = os.Stdout
	} else {
		// Setup log file
		if err := fs.EnsureDir(logFileName); err != nil {
			tracer(TargetBuildFailed, err)
			e.logError(targetLogger, actualPrefix, err)
			if e.rs.Opts.bail {
				os.Exit(1)
			}
		}
		output, err := os.Create(logFileName)
		if err != nil {
			tracer(TargetBuildFailed, err)
			e.logError(targetLogger, actualPrefix, err)
			if e.rs.Opts.bail {
				os.Exit(1)
			}
		}
		defer output.Close()
		bufWriter := bufio.NewWriter(output)
		bufWriter.WriteString(fmt.Sprintf("%scache hit, replaying output %s\n", actualPrefix, ui.Dim(hash)))
		defer bufWriter.Flush()
		if e.rs.Opts.cacheMissLogsMode == NoLogs || e.rs.Opts.cacheMissLogsMode == HashLogs {
			// only write to log file, not to stdout
			writer = bufWriter
		} else {
			writer = io.MultiWriter(os.Stdout, bufWriter)
		}
	}

	logger := log.New(writer, "", 0)
	// Setup a streamer that we'll pipe cmd.Stdout to
	logStreamerOut := logstreamer.NewLogstreamer(logger, actualPrefix, false)
	// Setup a streamer that we'll pipe cmd.Stderr to.
	logStreamerErr := logstreamer.NewLogstreamer(logger, actualPrefix, false)
	cmd.Stderr = logStreamerErr
	cmd.Stdout = logStreamerOut
	// Flush/Reset any error we recorded
	logStreamerErr.FlushRecord()
	logStreamerOut.FlushRecord()

	// Run the command
	if err := e.processes.Exec(cmd); err != nil {
		// if we already know we're in the process of exiting,
		// we don't need to record an error to that effect.
		if errors.Is(err, process.ErrClosing) {
			return nil
		}
		tracer(TargetBuildFailed, err)
		targetLogger.Error("Error: command finished with error: %w", err)
		if e.rs.Opts.bail {
			if e.rs.Opts.stream {
				targetUi.Error(fmt.Sprintf("Error: command finished with error: %s", err))
			} else {
				f, err := os.Open(filepath.Join(e.rs.Opts.cwd, logFileName))
				if err != nil {
					targetUi.Warn(fmt.Sprintf("failed reading logs: %v", err))
				}
				defer f.Close()
				scan := bufio.NewScanner(f)
				e.ui.Error("")
				e.ui.Error(util.Sprintf("%s ${RED}%s finished with error${RESET}", ui.ERROR_PREFIX, util.GetTaskId(pt.pkg.Name, pt.task)))
				e.ui.Error("")
				for scan.Scan() {
					e.ui.Output(util.Sprintf("${RED}%s:%s: ${RESET}%s", pt.pkg.Name, pt.task, scan.Bytes())) //Writing to Stdout
				}
			}
			e.processes.Close()
		} else {
			if e.rs.Opts.stream {
				targetUi.Warn("command finished with error, but continuing...")
			}
		}
		return err
	}

	// Cache command outputs
	if e.rs.Opts.cache && (pt.pipeline.Cache == nil || *pt.pipeline.Cache) {
		outputs := pt.HashableOutputs()
		targetLogger.Debug("caching output", "outputs", outputs)
		ignore := []string{}
		filesToBeCached := globby.GlobFiles(pt.pkg.Dir, outputs, ignore)
		if err := e.turboCache.Put(pt.pkg.Dir, hash, int(time.Since(cmdTime).Milliseconds()), filesToBeCached); err != nil {
			e.logError(targetLogger, "", fmt.Errorf("error caching output: %w", err))
		}
	}

	// Clean up tracing
	tracer(TargetBuilt, nil)
	targetLogger.Debug("done", "status", "complete", "duration", time.Since(cmdTime))
	return nil
}

func (c *RunCommand) generateDotGraph(taskGraph *dag.AcyclicGraph, outputFilename string) error {
	graphString := string(taskGraph.Dot(&dag.DotOpts{
		Verbose:    true,
		DrawCycles: true,
	}))
	ext := filepath.Ext(outputFilename)
	if ext == ".html" {
		f, err := os.Create(outputFilename)
		if err != nil {
			return fmt.Errorf("error writing graph: %w", err)
		}
		defer f.Close()
		f.WriteString(`<!DOCTYPE html>
    <html>
    <head>
      <meta charset="utf-8">
      <title>Graph</title>
    </head>
    <body>
      <script src="https://cdn.jsdelivr.net/npm/viz.js@2.1.2-pre.1/viz.js"></script>
      <script src="https://cdn.jsdelivr.net/npm/viz.js@2.1.2-pre.1/full.render.js"></script>
      <script>`)
		f.WriteString("const s = `" + graphString + "`.replace(/\\_\\_\\_ROOT\\_\\_\\_/g, \"Root\").replace(/\\[root\\]/g, \"\");new Viz().renderSVGElement(s).then(el => document.body.appendChild(el)).catch(e => console.error(e));")
		f.WriteString(`
    </script>
  </body>
  </html>`)
		c.Ui.Output("")
		c.Ui.Output(fmt.Sprintf("✔ Generated task graph in %s", ui.Bold(outputFilename)))
		if ui.IsTTY {
			browser.OpenBrowser(outputFilename)
		}
		return nil
	}
	hasDot := hasGraphViz()
	if hasDot {
		dotArgs := []string{"-T" + ext[1:], "-o", outputFilename}
		cmd := exec.Command("dot", dotArgs...)
		cmd.Stdin = strings.NewReader(graphString)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("could not generate task graphfile %v:  %w", outputFilename, err)
		} else {
			c.Ui.Output("")
			c.Ui.Output(fmt.Sprintf("✔ Generated task graph in %s", ui.Bold(outputFilename)))
		}
	} else {
		c.Ui.Output("")
		c.Ui.Warn(color.New(color.FgYellow, color.Bold, color.ReverseVideo).Sprint(" WARNING ") + color.YellowString(" `turbo` uses Graphviz to generate an image of your\ngraph, but Graphviz isn't installed on this machine.\n\nYou can download Graphviz from https://graphviz.org/download.\n\nIn the meantime, you can use this string output with an\nonline Dot graph viewer."))
		c.Ui.Output("")
		c.Ui.Output(graphString)
	}
	return nil
}

type packageTask struct {
	taskID      string
	task        string
	packageName string
	pkg         *fs.PackageJSON
	pipeline    *fs.Pipeline
}

func (pt *packageTask) ExternalOutputs() []string {
	if pt.pipeline.Outputs == nil {
		return []string{"dist/**/*", "build/**/*"}
	}
	return pt.pipeline.Outputs
}

func (pt *packageTask) RepoRelativeLogFile() string {
	return filepath.Join(pt.pkg.Dir, ".turbo", fmt.Sprintf("turbo-%v.log", pt.task))
}

func (pt *packageTask) HashableOutputs() []string {
	outputs := []string{fmt.Sprintf(".turbo/turbo-%v.log", pt.task)}
	outputs = append(outputs, pt.ExternalOutputs()...)
	return outputs
}

func (pt *packageTask) hash(args []string, logger hclog.Logger) (string, error) {
	// Hash ---------------------------------------------
	outputs := pt.HashableOutputs()
	logger.Debug("task output globs", "outputs", outputs)

	// Hash the task-specific environment variables found in the dependsOnKey in the pipeline
	var hashableEnvVars []string
	var hashableEnvPairs []string
	if len(pt.pipeline.DependsOn) > 0 {
		for _, v := range pt.pipeline.DependsOn {
			if strings.Contains(v, ENV_PIPELINE_DELIMITER) {
				trimmed := strings.TrimPrefix(v, ENV_PIPELINE_DELIMITER)
				hashableEnvPairs = append(hashableEnvPairs, fmt.Sprintf("%v=%v", trimmed, os.Getenv(trimmed)))
				hashableEnvVars = append(hashableEnvVars, trimmed)
			}
		}
		sort.Strings(hashableEnvVars) // always sort them
	}
	logger.Debug("hashable env vars", "vars", hashableEnvVars)
	hashable := struct {
		Hash             string
		Task             string
		Outputs          []string
		PassThruArgs     []string
		HashableEnvPairs []string
	}{
		Hash:             pt.pkg.Hash,
		Task:             pt.task,
		Outputs:          outputs,
		PassThruArgs:     args,
		HashableEnvPairs: hashableEnvPairs,
	}
	return fs.HashObject(hashable)
}

func (g *completeGraph) getPackageTaskVisitor(visitor func(pt *packageTask) error) func(taskID string) error {
	return func(taskID string) error {
		name, task := util.GetPackageTaskFromId(taskID)
		pkg := g.PackageInfos[name]
		// first check for package-tasks
		pipeline, ok := g.Pipeline[fmt.Sprintf("%v", taskID)]
		if !ok {
			// then check for regular tasks
			altpipe, notcool := g.Pipeline[task]
			// if neither, then bail
			if !notcool && !ok {
				return nil
			}
			// override if we need to...
			pipeline = altpipe
		}
		return visitor(&packageTask{
			taskID:      taskID,
			task:        task,
			packageName: name,
			pkg:         pkg,
			pipeline:    &pipeline,
		})
	}
}