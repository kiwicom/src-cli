package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/pkg/errors"
	"github.com/sourcegraph/src-cli/internal/batches"
	"github.com/sourcegraph/src-cli/internal/batches/git"
	"github.com/sourcegraph/src-cli/internal/batches/log"
	"github.com/sourcegraph/src-cli/internal/batches/workspace"

	yamlv3 "gopkg.in/yaml.v3"
)

// stepExecutionResult is the executionResult after executing the step with the
// given index in task.Steps.
type stepExecutionResult struct {
	// StepIndex is the index of the step in task.Steps
	StepIndex int `json:"stepIndex"`
	// Diff is the cumulative `git diff` after executing the Step.
	Diff []byte `json:"diff"`
	// Outputs is a copy of the Outputs after executing the Step.
	Outputs map[string]interface{} `json:"outputs"`
	// PreviousStepResult is the StepResult of the step before Step, if Step !=
	// 0.
	PreviousStepResult StepResult `json:"previousStepResult"`
}

type executionResult struct {
	// Diff is the produced by executing all steps.
	Diff string `json:"diff"`

	// ChangedFiles are files that have been changed by all steps.
	ChangedFiles *git.Changes `json:"changedFiles"`

	// Outputs are the outputs produced by all steps.
	Outputs map[string]interface{} `json:"outputs"`

	// Path relative to the repository's root directory in which the steps
	// have been executed.
	// No leading slashes. Root directory is blank string.
	Path string
}

type executionOpts struct {
	wc workspace.Creator

	task *Task

	tempDir string

	logger         log.TaskLogger
	reportProgress func(string)
}

func runSteps(ctx context.Context, opts *executionOpts) (result executionResult, stepResults []stepExecutionResult, err error) {
	opts.reportProgress("Downloading archive")
	err = opts.task.Archive.Fetch(ctx)
	if err != nil {
		return executionResult{}, nil, errors.Wrap(err, "fetching repo")
	}
	defer opts.task.Archive.Close()

	opts.reportProgress("Initializing workspace")
	workspace, err := opts.wc.Create(ctx, opts.task.Repository, opts.task.Steps, opts.task.Archive)
	if err != nil {
		return executionResult{}, nil, errors.Wrap(err, "creating workspace")
	}
	defer workspace.Close(ctx)

	var (
		execResult = executionResult{
			Diff:         "",
			ChangedFiles: &git.Changes{},
			Outputs:      make(map[string]interface{}),
			Path:         opts.task.Path,
		}
		previousStepResult StepResult
		startStep          int
	)

	if opts.task.CachedResultFound {
		// Set the Outputs to the cached outputs
		execResult.Outputs = opts.task.CachedResult.Outputs

		startStep = opts.task.CachedResult.StepIndex + 1

		// If we have cached results and don't need to execute any more steps,
		// we can quit
		if startStep == len(opts.task.Steps) {
			changes, err := git.ChangesInDiff(opts.task.CachedResult.Diff)
			if err != nil {
				return execResult, nil, errors.Wrap(err, "parsing cached step diff")
			}

			execResult.Diff = string(opts.task.CachedResult.Diff)
			execResult.ChangedFiles = &changes
			stepResults = append(stepResults, opts.task.CachedResult)

			return execResult, stepResults, nil
		}

		switch startStep {
		case 1:
			opts.reportProgress("Skipping step 1. Found cached result.")
		default:
			opts.reportProgress(fmt.Sprintf("Skipping steps 1 to %d. Found cached results.", startStep))
		}
	}

	for i := startStep; i < len(opts.task.Steps); i++ {
		step := opts.task.Steps[i]

		stepContext := StepContext{
			BatchChange: *opts.task.BatchChangeAttributes,
			Repository:  *opts.task.Repository,
			Outputs:     execResult.Outputs,
			Steps: StepsContext{
				Path:    execResult.Path,
				Changes: previousStepResult.Files,
			},
			PreviousStep: previousStepResult,
		}

		if opts.task.CachedResultFound && i == startStep {
			previousStepResult = opts.task.CachedResult.PreviousStepResult

			stepContext.PreviousStep = previousStepResult
			stepContext.Steps.Changes = previousStepResult.Files
			stepContext.Outputs = opts.task.CachedResult.Outputs

			if err := workspace.ApplyDiff(ctx, []byte(opts.task.CachedResult.Diff)); err != nil {
				return execResult, nil, errors.Wrap(err, "getting changed files in step")
			}
		}

		cond, err := evalStepCondition(step.IfCondition(), &stepContext)
		if err != nil {
			return execResult, nil, errors.Wrap(err, "evaluating step condition")
		}
		if !cond {
			opts.reportProgress(fmt.Sprintf("Skipping step %d", i+1))
			continue
		}

		stdoutBuffer, stderrBuffer, err := executeSingleStep(ctx, opts, workspace, i, step, &stepContext)
		if err != nil {
			return execResult, nil, err
		}

		changes, err := workspace.Changes(ctx)
		if err != nil {
			return execResult, nil, errors.Wrap(err, "getting changed files in step")
		}

		result := StepResult{Files: changes, Stdout: &stdoutBuffer, Stderr: &stderrBuffer}

		// Set stepContext.Step to current step's results before rendering outputs
		stepContext.Step = result
		// Render and evaluate outputs
		if err := setOutputs(step.Outputs, execResult.Outputs, &stepContext); err != nil {
			return execResult, nil, errors.Wrap(err, "setting step outputs")
		}

		// Get the current diff and store that away as the per-step result.
		stepDiff, err := workspace.Diff(ctx)
		if err != nil {
			return execResult, nil, errors.Wrap(err, "getting diff produced by step")
		}
		stepResult := stepExecutionResult{
			StepIndex:          i,
			Diff:               stepDiff,
			Outputs:            make(map[string]interface{}),
			PreviousStepResult: stepContext.PreviousStep,
		}
		for k, v := range execResult.Outputs {
			stepResult.Outputs[k] = v
		}
		stepResults = append(stepResults, stepResult)
		previousStepResult = result
	}

	opts.reportProgress("Calculating diff")
	diffOut, err := workspace.Diff(ctx)
	if err != nil {
		return execResult, nil, errors.Wrap(err, "git diff failed")
	}

	execResult.Diff = string(diffOut)
	execResult.ChangedFiles = previousStepResult.Files

	return execResult, stepResults, err
}

const workDir = "/work"

func executeSingleStep(
	ctx context.Context,
	opts *executionOpts,
	workspace workspace.Workspace,
	i int,
	step batches.Step,
	stepContext *StepContext,
) (bytes.Buffer, bytes.Buffer, error) {
	// ----------
	// PREPARATION
	// ----------
	opts.reportProgress(fmt.Sprintf("Preparing step %d", i+1))

	cidFile, cleanup, err := createCidFile(ctx, opts.tempDir, opts.task.Repository.Slug())
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, err
	}
	defer cleanup()

	// We need to grab the digest for the exact image we're using.
	digest, err := step.ImageDigest(ctx)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, errors.Wrapf(err, "getting digest for %v", step.DockerImage())
	}

	// For now, we only support shell scripts provided via the Run field.
	shell, containerTemp, err := probeImageForShell(ctx, digest)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, errors.Wrapf(err, "probing image %q for shell", step.DockerImage())
	}

	runScriptFile, runScript, cleanup, err := createRunScriptFile(ctx, opts.tempDir, step.Run, stepContext)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, err
	}

	// Parse and render the step.Files.
	filesToMount, cleanup, err := createFilesToMount(opts.tempDir, step, stepContext)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, err
	}
	defer cleanup()

	// Resolve step.Env given the current environment.
	stepEnv, err := step.Env.Resolve(os.Environ())
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, errors.Wrap(err, "resolving step environment")
	}

	// Render the step.Env variables as templates.
	env, err := renderStepMap(stepEnv, stepContext)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, errors.Wrap(err, "parsing step environment")
	}

	// ----------
	// EXECUTION
	// ----------
	opts.reportProgress(runScript)

	workspaceOpts, err := workspace.DockerRunOpts(ctx, workDir)
	if err != nil {
		return bytes.Buffer{}, bytes.Buffer{}, errors.Wrap(err, "getting Docker options for workspace")
	}

	// Where should we execute the steps.run script?
	scriptWorkDir := workDir
	if opts.task.Path != "" {
		scriptWorkDir = workDir + "/" + opts.task.Path
	}

	args := append([]string{
		"run",
		"--rm",
		"--init",
		"--cidfile", cidFile,
		"--workdir", scriptWorkDir,
		"--mount", fmt.Sprintf("type=bind,source=%s,target=%s,ro", runScriptFile, containerTemp),
	}, workspaceOpts...)

	for target, source := range filesToMount {
		args = append(args, "--mount", fmt.Sprintf("type=bind,source=%s,target=%s,ro", source.Name(), target))
	}

	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, "--entrypoint", shell)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Args = append(cmd.Args, "--", digest, containerTemp)
	if dir := workspace.WorkDir(); dir != nil {
		cmd.Dir = *dir
	}

	var stdoutBuffer, stderrBuffer bytes.Buffer

	cmd.Stdout = io.MultiWriter(&stdoutBuffer, opts.logger.PrefixWriter("stdout"))
	cmd.Stderr = io.MultiWriter(&stderrBuffer, opts.logger.PrefixWriter("stderr"))

	opts.logger.Logf("[Step %d] run: %q, container: %q", i+1, step.Run, step.Container)
	opts.logger.Logf("[Step %d] full command: %q", i+1, strings.Join(cmd.Args, " "))

	t0 := time.Now()
	err = cmd.Run()
	elapsed := time.Since(t0).Round(time.Millisecond)
	if err != nil {
		opts.logger.Logf("[Step %d] took %s; error running Docker container: %+v", i+1, elapsed, err)

		return stdoutBuffer, stderrBuffer, stepFailedErr{
			Err:         err,
			Args:        cmd.Args,
			Run:         runScript,
			Container:   step.Container,
			TmpFilename: containerTemp,
			Stdout:      strings.TrimSpace(stdoutBuffer.String()),
			Stderr:      strings.TrimSpace(stderrBuffer.String()),
		}
	}

	opts.logger.Logf("[Step %d] complete in %s", i+1, elapsed)
	return stdoutBuffer, stderrBuffer, nil
}

func setOutputs(stepOutputs batches.Outputs, global map[string]interface{}, stepCtx *StepContext) error {
	for name, output := range stepOutputs {
		var value bytes.Buffer

		if err := renderStepTemplate("outputs-"+name, output.Value, &value, stepCtx); err != nil {
			return errors.Wrap(err, "parsing step run")
		}

		switch output.Format {
		case "yaml":
			var out interface{}
			// We use yamlv3 here, because it unmarshals YAML into
			// map[string]interface{} which we need to serialize it back to
			// JSON when we cache the results.
			// See https://github.com/go-yaml/yaml/issues/139 for context
			if err := yamlv3.NewDecoder(&value).Decode(&out); err != nil {
				return err
			}
			global[name] = out
		case "json":
			var out interface{}
			if err := json.NewDecoder(&value).Decode(&out); err != nil {
				return err
			}
			global[name] = out
		default:
			global[name] = value.String()
		}
	}

	return nil
}

func probeImageForShell(ctx context.Context, image string) (shell, tempfile string, err error) {
	// We need to know two things to be able to run a shell script:
	//
	// 1. Which shell is available. We're going to look for /bin/bash and then
	//    /bin/sh, in that order. (Sorry, tcsh users.)
	// 2. Where to put the shell script in the container so that we don't
	//    clobber any actual user data.
	//
	// We can do these together: although it's not part of POSIX proper, every
	// *nix made in the last decade or more has mktemp(1) available. We know
	// that mktemp will give us a file name that doesn't exist in the image if
	// we run it as part of the command. We can also probe for the shell at the
	// same time by trying to run /bin/bash -c mktemp,
	// followed by /bin/sh -c mktemp.

	// We'll also set up our error.
	err = new(multierror.Error)

	// Now we can iterate through our shell options and try to run mktemp with
	// them.
	for _, shell = range []string{"/bin/bash", "/bin/sh"} {
		stdout := new(bytes.Buffer)
		stderr := new(bytes.Buffer)

		args := []string{"run", "--rm", "--entrypoint", shell, image, "-c", "mktemp"}

		cmd := exec.CommandContext(ctx, "docker", args...)
		cmd.Stdout = stdout
		cmd.Stderr = stderr

		if runErr := cmd.Run(); runErr != nil {
			err = multierror.Append(err, errors.Wrapf(runErr, "probing shell %q:\n%s", shell, stderr.String()))
		} else {
			// Even if there were previous errors, we can now ignore them.
			err = nil
			tempfile = strings.TrimSpace(stdout.String())
			return
		}
	}

	// If we got here, then all the attempts to probe the shell failed. Let's
	// admit defeat and return. At least err is already in place.
	return
}

// createFilesToMount creates temporary files with the contents of Step.Files
// that are to be mounted into the container that executes the step.
func createFilesToMount(tempDir string, step batches.Step, stepContext *StepContext) (map[string]*os.File, func(), error) {
	// Parse and render the step.Files.
	files, err := renderStepMap(step.Files, stepContext)
	if err != nil {
		return nil, nil, errors.Wrap(err, "parsing step files")
	}

	var toCleanup []string
	cleanup := func() {
		for _, fname := range toCleanup {
			os.Remove(fname)
		}
	}

	// Create temp files with the rendered content of step.Files so that we
	// can mount them into the container.
	filesToMount := make(map[string]*os.File, len(files))
	for name, content := range files {
		fp, err := ioutil.TempFile(tempDir, "")
		if err != nil {
			return nil, cleanup, errors.Wrap(err, "creating temporary file")
		}
		toCleanup = append(toCleanup, fp.Name())

		if _, err := fp.WriteString(content); err != nil {
			return nil, cleanup, errors.Wrap(err, "writing to temporary file")
		}

		if err := fp.Close(); err != nil {
			return nil, cleanup, errors.Wrap(err, "closing temporary file")
		}

		filesToMount[name] = fp
	}

	return filesToMount, cleanup, nil
}

// createRunScriptFile creates a temporary file and renders stepRun into it.
//
// It returns the location of the file, its content, a function to cleanup the file and possible errors.
func createRunScriptFile(ctx context.Context, tempDir string, stepRun string, stepCtx *StepContext) (string, string, func(), error) {
	// Set up a temporary file on the host filesystem to contain the
	// script.
	runScriptFile, err := ioutil.TempFile(tempDir, "")
	if err != nil {
		return "", "", nil, errors.Wrap(err, "creating temporary file")
	}
	cleanup := func() { os.Remove(runScriptFile.Name()) }

	// Parse step.Run as a template and render it into a buffer and the
	// temp file we just created.
	var runScript bytes.Buffer
	out := io.MultiWriter(&runScript, runScriptFile)
	if err := renderStepTemplate("step-run", stepRun, out, stepCtx); err != nil {
		return "", "", nil, errors.Wrap(err, "parsing step run")
	}

	if err := runScriptFile.Close(); err != nil {
		return "", "", nil, errors.Wrap(err, "closing temporary file")
	}

	// This file needs to be readable within the container regardless of the
	// user the container is running as, so we'll set the appropriate group
	// and other bits to make it so.
	//
	// A fun note: although os.File exposes a Chmod() method, we can't
	// unconditionally use it because Windows cannot change the attributes
	// of an open file. Rather than going to the trouble of having
	// conditionally compiled files here, instead we'll just wait until the
	// file is closed to twiddle the permission bits. Which is now!
	if err := os.Chmod(runScriptFile.Name(), 0644); err != nil {
		return "", "", nil, errors.Wrap(err, "setting permissions on the temporary file")
	}

	return runScriptFile.Name(), runScript.String(), cleanup, nil
}

// createCidFile creates a temporary file that will contain the container ID
// when executing steps.
// It returns the location of the file and a function that cleans up the
// file.
func createCidFile(ctx context.Context, tempDir string, repoSlug string) (string, func(), error) {
	// Find a location that we can use for a cidfile, which will contain the
	// container ID that is used below. We can then use this to remove the
	// container on a successful run, rather than leaving it dangling.
	cidFile, err := ioutil.TempFile(tempDir, repoSlug+"-container-id")
	if err != nil {
		return "", nil, errors.Wrap(err, "Creating a CID file failed")
	}

	// However, Docker will fail if the cidfile actually exists, so we need
	// to remove it. Because Windows can't remove open files, we'll first
	// close it, even though that's unnecessary elsewhere.
	cidFile.Close()
	if err = os.Remove(cidFile.Name()); err != nil {
		return "", nil, errors.Wrap(err, "removing cidfile")
	}

	// Since we went to all that effort, we can now defer a function that
	// uses the cidfile to clean up after this function is done.
	cleanup := func() {
		cid, err := ioutil.ReadFile(cidFile.Name())
		_ = os.Remove(cidFile.Name())
		if err == nil {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			_ = exec.CommandContext(ctx, "docker", "rm", "-f", "--", string(cid)).Run()
		}
	}

	return cidFile.Name(), cleanup, nil
}

type stepFailedErr struct {
	Run       string
	Container string

	TmpFilename string

	Args   []string
	Stdout string
	Stderr string

	Err error
}

func (e stepFailedErr) Cause() error { return e.Err }

func (e stepFailedErr) Error() string {
	var out strings.Builder

	fmtRun := func(run string) string {
		lines := strings.Split(run, "\n")
		if len(lines) == 1 {
			return lines[0]
		}
		return lines[0] + fmt.Sprintf("\n\t(... and %d more lines)", len(lines)-1)
	}

	out.WriteString(fmt.Sprintf("run: %s\ncontainer: %s\n", fmtRun(e.Run), e.Container))

	printOutput := func(output string) {
		for _, line := range strings.Split(output, "\n") {
			if e.TmpFilename != "" {
				line = strings.ReplaceAll(line, e.TmpFilename+": ", "")
			}
			out.WriteString("\t" + line + "\n")
		}
	}

	if len(e.Stdout) > 0 {
		out.WriteString("\nstandard out:\n")
		printOutput(e.Stdout)
	}

	if len(e.Stderr) > 0 {
		out.WriteString("\nstandard error:\n")
		printOutput(e.Stderr)
	}

	if exitErr, ok := e.Err.(*exec.ExitError); ok {
		out.WriteString(fmt.Sprintf("\nCommand failed with exit code %d.", exitErr.ExitCode()))
	} else {
		out.WriteString(fmt.Sprintf("\nCommand failed: %s", e.Err))
	}

	return out.String()
}

func (e stepFailedErr) SingleLineError() string {
	out := e.Err.Error()
	if len(e.Stderr) > 0 {
		out = e.Stderr
	}

	return strings.Split(out, "\n")[0]
}
