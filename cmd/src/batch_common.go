package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sourcegraph/src-cli/internal/api"
	"github.com/sourcegraph/src-cli/internal/batches"
	"github.com/sourcegraph/src-cli/internal/batches/executor"
	"github.com/sourcegraph/src-cli/internal/batches/graphql"
	"github.com/sourcegraph/src-cli/internal/batches/service"
	"github.com/sourcegraph/src-cli/internal/batches/ui"
	"github.com/sourcegraph/src-cli/internal/batches/workspace"
	"github.com/sourcegraph/src-cli/internal/cmderrors"
)

type batchExecuteFlags struct {
	allowUnsupported bool
	allowIgnored     bool
	api              *api.Flags
	apply            bool
	cacheDir         string
	tempDir          string
	clearCache       bool
	file             string
	keepLogs         bool
	namespace        string
	parallelism      int
	timeout          time.Duration
	workspace        string
	cleanArchives    bool
	skipErrors       bool

	// EXPERIMENTAL
	textOnly bool
}

func newBatchExecuteFlags(flagSet *flag.FlagSet, cacheDir, tempDir string) *batchExecuteFlags {
	caf := &batchExecuteFlags{
		api: api.NewFlags(flagSet),
	}

	flagSet.BoolVar(
		&caf.textOnly, "text-only", false,
		"INTERNAL USE ONLY. EXPERIMENTAL. Switches off the TUI to only print JSON lines.",
	)
	flagSet.BoolVar(
		&caf.allowUnsupported, "allow-unsupported", false,
		"Allow unsupported code hosts.",
	)
	flagSet.BoolVar(
		&caf.allowIgnored, "force-override-ignore", false,
		"Do not ignore repositories that have a .batchignore file.",
	)
	flagSet.BoolVar(
		&caf.apply, "apply", false,
		"Ignored.",
	)
	flagSet.StringVar(
		&caf.cacheDir, "cache", cacheDir,
		"Directory for caching results and repository archives.",
	)
	flagSet.BoolVar(
		&caf.clearCache, "clear-cache", false,
		"If true, clears the execution cache and executes all steps anew.",
	)
	flagSet.StringVar(
		&caf.tempDir, "tmp", tempDir,
		"Directory for storing temporary data, such as log files. Default is /tmp. Can also be set with environment variable SRC_BATCH_TMP_DIR; if both are set, this flag will be used and not the environment variable.",
	)
	flagSet.StringVar(
		&caf.file, "f", "",
		"The batch spec file to read.",
	)
	flagSet.BoolVar(
		&caf.keepLogs, "keep-logs", false,
		"Retain logs after executing steps.",
	)
	flagSet.StringVar(
		&caf.namespace, "namespace", "",
		"The user or organization namespace to place the batch change within. Default is the currently authenticated user.",
	)
	flagSet.StringVar(&caf.namespace, "n", "", "Alias for -namespace.")

	flagSet.IntVar(
		&caf.parallelism, "j", runtime.GOMAXPROCS(0),
		"The maximum number of parallel jobs. Default is GOMAXPROCS.",
	)
	flagSet.DurationVar(
		&caf.timeout, "timeout", 60*time.Minute,
		"The maximum duration a single batch spec step can take.",
	)
	flagSet.BoolVar(
		&caf.cleanArchives, "clean-archives", true,
		"If true, deletes downloaded repository archives after executing batch spec steps.",
	)
	flagSet.BoolVar(
		&caf.skipErrors, "skip-errors", false,
		"If true, errors encountered while executing steps in a repository won't stop the execution of the batch spec but only cause that repository to be skipped.",
	)

	flagSet.StringVar(
		&caf.workspace, "workspace", "auto",
		`Workspace mode to use ("auto", "bind", or "volume")`,
	)

	flagSet.BoolVar(verbose, "v", false, "print verbose output")

	return caf
}

func batchDefaultCacheDir() string {
	uc, err := os.UserCacheDir()
	if err != nil {
		return ""
	}

	// Check if there's an old campaigns cache directory but not a new batch
	// directory: if so, we should rename the old directory and carry on.
	//
	// TODO(campaigns-deprecation): we can remove this migration shim after June
	// 2021.
	old := path.Join(uc, "sourcegraph", "campaigns")
	dir := path.Join(uc, "sourcegraph", "batch")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if _, err := os.Stat(old); os.IsExist(err) {
			// We'll just try to do this without checking for an error: if it
			// fails, we'll carry on and let the normal cache directory handling
			// logic take care of it.
			os.Rename(old, dir)
		}
	}

	return dir
}

// batchDefaultTempDirPrefix returns the prefix to be passed to ioutil.TempFile.
// If one of the environment variables SRC_BATCH_TMP_DIR or
// SRC_CAMPAIGNS_TMP_DIR is set, that is used as the prefix. Otherwise we use
// "/tmp".
func batchDefaultTempDirPrefix() string {
	// TODO(campaigns-deprecation): we can remove this migration shim in
	// Sourcegraph 4.0.
	for _, env := range []string{"SRC_BATCH_TMP_DIR", "SRC_CAMPAIGNS_TMP_DIR"} {
		if p := os.Getenv(env); p != "" {
			return p
		}
	}

	// On macOS, we use an explicit prefix for our temp directories, because
	// otherwise Go would use $TMPDIR, which is set to `/var/folders` per
	// default on macOS. But Docker for Mac doesn't have `/var/folders` in its
	// default set of shared folders, but it does have `/tmp` in there.
	if runtime.GOOS == "darwin" {
		return "/tmp"
	}

	return os.TempDir()
}

func batchOpenFileFlag(flag *string) (io.ReadCloser, error) {
	if flag == nil || *flag == "" || *flag == "-" {
		return os.Stdin, nil
	}

	file, err := os.Open(*flag)
	if err != nil {
		return nil, errors.Wrapf(err, "cannot open file %q", *flag)
	}
	return file, nil
}

type executeBatchSpecOpts struct {
	flags *batchExecuteFlags

	applyBatchSpec bool

	ui ui.ExecUI

	client api.Client
}

// executeBatchSpec performs all the steps required to upload the batch spec to
// Sourcegraph, including execution as needed and applying the resulting batch
// spec if specified.
func executeBatchSpec(ctx context.Context, opts executeBatchSpecOpts) (err error) {
	defer func() {
		if err != nil {
			opts.ui.ExecutionError(err)
		}
	}()

	svc := service.New(&service.Opts{
		AllowUnsupported: opts.flags.allowUnsupported,
		AllowIgnored:     opts.flags.allowIgnored,
		Client:           opts.client,
	})

	if err := svc.DetermineFeatureFlags(ctx); err != nil {
		return err
	}

	if err := checkExecutable("git", "version"); err != nil {
		return err
	}

	if err := checkExecutable("docker", "version"); err != nil {
		return err
	}

	// Parse flags and build up our service and executor options.
	opts.ui.ParsingBatchSpec()
	batchSpec, rawSpec, err := batchParseSpec(&opts.flags.file, svc)
	if err != nil {
		if merr, ok := err.(*multierror.Error); ok {
			opts.ui.ParsingBatchSpecFailure(merr)
			return cmderrors.ExitCode(2, nil)
		} else {
			// This shouldn't happen; let's just punt and let the normal
			// rendering occur.
			return err
		}
	}
	opts.ui.ParsingBatchSpecSuccess()

	opts.ui.ResolvingNamespace()
	namespace, err := svc.ResolveNamespace(ctx, opts.flags.namespace)
	if err != nil {
		return err
	}
	opts.ui.ResolvingNamespaceSuccess(namespace)

	opts.ui.PreparingContainerImages()
	err = svc.SetDockerImages(ctx, batchSpec, opts.ui.PreparingContainerImagesProgress)
	if err != nil {
		return err
	}
	opts.ui.PreparingContainerImagesSuccess()

	opts.ui.DeterminingWorkspaceCreatorType()
	workspaceCreator := workspace.NewCreator(ctx, opts.flags.workspace, opts.flags.cacheDir, opts.flags.tempDir, batchSpec.Steps)
	if workspaceCreator.Type() == workspace.CreatorTypeVolume {
		_, err = svc.EnsureImage(ctx, workspace.DockerVolumeWorkspaceImage)
		if err != nil {
			return err
		}
	}
	opts.ui.DeterminingWorkspaceCreatorTypeSuccess(workspaceCreator.Type())

	opts.ui.ResolvingRepositories()
	repos, err := svc.ResolveRepositories(ctx, batchSpec)
	if err != nil {
		if repoSet, ok := err.(batches.UnsupportedRepoSet); ok {
			opts.ui.ResolvingRepositoriesDone(repos, repoSet, nil)
		} else if repoSet, ok := err.(batches.IgnoredRepoSet); ok {
			opts.ui.ResolvingRepositoriesDone(repos, nil, repoSet)
		} else {
			return errors.Wrap(err, "resolving repositories")
		}
	} else {
		opts.ui.ResolvingRepositoriesDone(repos, nil, nil)
	}

	opts.ui.DeterminingWorkspaces()
	tasks, err := svc.BuildTasks(ctx, repos, batchSpec)
	if err != nil {
		return err
	}
	opts.ui.DeterminingWorkspacesSuccess(len(tasks))

	// EXECUTION OF TASKS
	coord := svc.NewCoordinator(executor.NewCoordinatorOpts{
		Creator:       workspaceCreator,
		CacheDir:      opts.flags.cacheDir,
		ClearCache:    opts.flags.clearCache,
		SkipErrors:    opts.flags.skipErrors,
		CleanArchives: opts.flags.cleanArchives,
		Parallelism:   opts.flags.parallelism,
		Timeout:       opts.flags.timeout,
		KeepLogs:      opts.flags.keepLogs,
		TempDir:       opts.flags.tempDir,
	})

	opts.ui.CheckingCache()
	uncachedTasks, cachedSpecs, err := coord.CheckCache(ctx, tasks)
	if err != nil {
		return err
	}
	opts.ui.CheckingCacheSuccess(len(cachedSpecs), len(uncachedTasks))

	taskExecUI := opts.ui.ExecutingTasks(*verbose, opts.flags.parallelism)
	freshSpecs, logFiles, err := coord.Execute(ctx, uncachedTasks, batchSpec, taskExecUI)
	if err != nil && !opts.flags.skipErrors {
		return err
	}
	taskExecUI.Success()
	if err != nil && opts.flags.skipErrors {
		opts.ui.ExecutingTasksSkippingErrors(err)
	}

	if len(logFiles) > 0 && opts.flags.keepLogs {
		opts.ui.LogFilesKept(logFiles)
	}

	specs := append(cachedSpecs, freshSpecs...)

	err = svc.ValidateChangesetSpecs(repos, specs)
	if err != nil {
		return err
	}

	ids := make([]graphql.ChangesetSpecID, len(specs))

	if len(specs) > 0 {
		opts.ui.UploadingChangesetSpecs(len(specs))

		for i, spec := range specs {
			id, err := svc.CreateChangesetSpec(ctx, spec)
			if err != nil {
				return err
			}
			ids[i] = id
			opts.ui.UploadingChangesetSpecsProgress(i+1, len(specs))
		}

		opts.ui.UploadingChangesetSpecsSuccess()
	} else {
		if len(repos) == 0 {
			opts.ui.NoChangesetSpecs()
		}
	}

	opts.ui.CreatingBatchSpec()
	id, url, err := svc.CreateBatchSpec(ctx, namespace, rawSpec, ids)
	opts.ui.CreatingBatchSpecSuccess()
	if err != nil {
		return opts.ui.CreatingBatchSpecError(err)
	}

	if opts.applyBatchSpec {
		opts.ui.ApplyingBatchSpec()
		batch, err := svc.ApplyBatchChange(ctx, id)
		if err != nil {
			return err
		}
		opts.ui.ApplyingBatchSpecSuccess(cfg.Endpoint + batch.URL)

	} else {
		opts.ui.PreviewBatchSpec(cfg.Endpoint + url)
	}

	return nil
}

// batchParseSpec parses and validates the given batch spec. If the spec has
// validation errors, they are returned.
func batchParseSpec(file *string, svc *service.Service) (*batches.BatchSpec, string, error) {
	f, err := batchOpenFileFlag(file)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	return svc.ParseBatchSpec(f)
}

func checkExecutable(cmd string, args ...string) error {
	if err := exec.Command(cmd, args...).Run(); err != nil {
		return fmt.Errorf(
			"failed to execute \"%s %s\":\n\t%s\n\n'src batch' require %q to be available.",
			cmd,
			strings.Join(args, " "),
			err,
			cmd,
		)
	}
	return nil
}

func contextCancelOnInterrupt(parent context.Context) (context.Context, func()) {
	ctx, ctxCancel := context.WithCancel(parent)
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		select {
		case <-c:
			ctxCancel()
		case <-ctx.Done():
		}
	}()

	return ctx, func() {
		signal.Stop(c)
		ctxCancel()
	}
}
