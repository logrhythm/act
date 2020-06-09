package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nektos/act/pkg/container"

	"github.com/nektos/act/pkg/common"
	"github.com/nektos/act/pkg/model"
	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"
)

// RunContext contains info about current job
type RunContext struct {
	Name          string
	Config        *Config
	Matrix        map[string]interface{}
	Run           *model.Run
	EventJSON     string
	Env           map[string]string
	ExtraPath     []string
	CurrentStep   string
	StepResults   map[string]*stepResult
	ExprEval      ExpressionEvaluator
	JobContainer  container.Container
	DINDContainer container.Container
}

func (rc *RunContext) String() string {
	return fmt.Sprintf("%s/%s", rc.Run.Workflow.Name, rc.Name)
}

type stepResult struct {
	Success bool              `json:"success"`
	Outputs map[string]string `json:"outputs"`
}

// GetEnv returns the env for the context
func (rc *RunContext) GetEnv() map[string]string {
	if rc.Env == nil {
		rc.Env = mergeMaps(rc.Config.Env, rc.Run.Workflow.Env, rc.Run.Job().Env)
	}
	return rc.Env
}

func (rc *RunContext) dindContainerName() string {
	return createContainerName("act", "dind", rc.String())
}

func (rc *RunContext) startDINDContainer() common.Executor {
	image := "docker:dind"

	return func(ctx context.Context) error {
		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof(s)
			} else {
				rawLogger.Debugf(s)
			}
			return true
		})

		common.Logger(ctx).Infof("\U0001f680  Start image=%s", image)
		name := rc.dindContainerName()

		envList := make([]string, 0)
		bindModifiers := ""
		if runtime.GOOS == "darwin" {
			bindModifiers = ":delegated"
		}

		envList = append(envList, fmt.Sprintf("%s=%s", "DOCKER_TLS_CERTDIR", "/certs"))

		binds := []string{
			// fmt.Sprintf("%s:%s", "/var/run/docker.sock", "/var/run/docker.sock"),
		}
		if rc.Config.BindWorkdir {
			binds = append(binds, fmt.Sprintf("%s:%s%s", rc.Config.Workdir, "/github/workspace", bindModifiers))
		}

		dindCertVolumeName := fmt.Sprintf("%s-cert", rc.dindContainerName())

		rc.DINDContainer = container.NewContainer(&container.NewContainerInput{
			Image: image,
			Name:  name,
			Env:   envList,
			Mounts: map[string]string{
				dindCertVolumeName:    "/certs/client",
				rc.jobContainerName(): "/github",
				"act-toolcache":       "/toolcache",
				"act-actions":         "/actions",
				"act-runner-home":     "/home/runner",
				"act-dind-imagecache": "/var/lib/docker/overlay2",
			},
			NetworkMode: "default",
			Binds:       binds,
			Stdout:      logWriter,
			Stderr:      logWriter,
			Privileged:  true,
		})

		return common.NewPipelineExecutor(
			rc.DINDContainer.Pull(false),
			rc.DINDContainer.Remove().IfBool(!rc.Config.ReuseContainers),
			rc.DINDContainer.Create(),
			rc.DINDContainer.Start(false),
		)(ctx)
	}
}

func (rc *RunContext) jobContainerName() string {
	return createContainerName("act", rc.String())
}

func (rc *RunContext) startJobContainer() common.Executor {
	image := rc.platformImage()

	return func(ctx context.Context) error {
		rawLogger := common.Logger(ctx).WithField("raw_output", true)
		logWriter := common.NewLineWriter(rc.commandHandler(ctx), func(s string) bool {
			if rc.Config.LogOutput {
				rawLogger.Infof(s)
			} else {
				rawLogger.Debugf(s)
			}
			return true
		})

		common.Logger(ctx).Infof("\U0001f680  Start image=%s", image)
		name := rc.jobContainerName()

		envList := make([]string, 0)
		bindModifiers := ""
		if runtime.GOOS == "darwin" {
			bindModifiers = ":delegated"
		}

		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TOOL_CACHE", "/opt/hostedtoolcache"))
		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_OS", "Linux"))
		envList = append(envList, fmt.Sprintf("%s=%s", "RUNNER_TEMP", "/tmp"))
		// envList = append(envList, fmt.Sprintf("%s=%s", "CI_LOCAL", "true"))
		envList = append(envList, fmt.Sprintf("%s=%s", "DOCKER_TLS_VERIFY", "1"))
		envList = append(envList, fmt.Sprintf("%s=%s", "DOCKER_CERT_PATH", "/certs/client"))
		envList = append(envList, fmt.Sprintf("%s=%s", "DOCKER_HOST", "tcp://docker:2376"))

		binds := []string{
			// fmt.Sprintf("%s:%s", "/var/run/docker.sock", "/var/run/docker.sock"),
		}
		if rc.Config.BindWorkdir {
			binds = append(binds, fmt.Sprintf("%s:%s%s", rc.Config.Workdir, "/github/workspace", bindModifiers))
		}

		dindCertVolumeName := fmt.Sprintf("%s-cert", rc.dindContainerName())

		rc.JobContainer = container.NewContainer(&container.NewContainerInput{
			Cmd:        nil,
			Entrypoint: []string{"/usr/bin/tail", "-f", "/dev/null"},
			WorkingDir: "/github/workspace",
			Image:      image,
			Name:       name,
			Env:        envList,
			Mounts: map[string]string{
				dindCertVolumeName: "/certs/client",
				name:               "/github",
				"act-toolcache":    "/toolcache",
				"act-actions":      "/actions",
				"act-runner-home":  "/home/runner",
			},
			NetworkMode: "default",
			Links: []string{
				fmt.Sprintf("%s:docker", rc.dindContainerName()),
				fmt.Sprintf("%s:deps.localdev.boreas.cloud", rc.dindContainerName()),
			},
			Binds:  binds,
			Stdout: logWriter,
			Stderr: logWriter,
		})

		var copyWorkspace bool
		var copyToPath string
		if !rc.Config.BindWorkdir {
			copyToPath, copyWorkspace = rc.localCheckoutPath()
			copyToPath = filepath.Join("/github/workspace", copyToPath)
		}

		return common.NewPipelineExecutor(
			rc.JobContainer.Pull(rc.Config.ForcePull),
			rc.JobContainer.Remove().IfBool(!rc.Config.ReuseContainers),
			rc.JobContainer.Create(),
			rc.JobContainer.Start(false),
			rc.JobContainer.CopyDir(copyToPath, rc.Config.Workdir+"/.", false).IfBool(copyWorkspace),
			rc.JobContainer.ChangeRemoteToHttps(copyToPath).IfBool(copyWorkspace),
			rc.JobContainer.Copy("/github/", &container.FileEntry{
				Name: "workflow/event.json",
				Mode: 0644,
				Body: rc.EventJSON,
			}, &container.FileEntry{
				Name: "home/.act",
				Mode: 0644,
				Body: "",
			}),
		)(ctx)
	}
}
func (rc *RunContext) execJobContainer(cmd []string, env map[string]string) common.Executor {
	return func(ctx context.Context) error {
		return rc.JobContainer.Exec(cmd, env)(ctx)
	}
}
func (rc *RunContext) stopJobContainer() common.Executor {
	return func(ctx context.Context) error {
		if rc.JobContainer != nil && !rc.Config.ReuseContainers {
			return rc.JobContainer.Remove().
				Then(rc.DINDContainer.Remove()).
				Then(container.NewDockerVolumeRemoveExecutor(rc.jobContainerName(), false))(ctx)
		}
		return nil
	}
}

// ActionCacheDir is for rc
func (rc *RunContext) ActionCacheDir() string {
	var xdgCache string
	var ok bool
	if xdgCache, ok = os.LookupEnv("XDG_CACHE_HOME"); !ok {
		if home, ok := os.LookupEnv("HOME"); ok {
			xdgCache = fmt.Sprintf("%s/.cache", home)
		}
	}
	return filepath.Join(xdgCache, "act")
}

// Executor returns a pipeline executor for all the steps in the job
func (rc *RunContext) Executor() common.Executor {
	steps := make([]common.Executor, 0)

	steps = append(steps, func(ctx context.Context) error {
		if len(rc.Matrix) > 0 {
			common.Logger(ctx).Infof("\U0001F9EA  Matrix: %v", rc.Matrix)
		}
		return nil
	})

	steps = append(steps, rc.startDINDContainer())
	steps = append(steps, rc.startJobContainer())

	for i, step := range rc.Run.Job().Steps {
		if step.ID == "" {
			step.ID = fmt.Sprintf("%d", i)
		}
		steps = append(steps, rc.newStepExecutor(step))
	}
	steps = append(steps, rc.stopJobContainer())

	return common.NewPipelineExecutor(steps...).If(rc.isEnabled)
}

func (rc *RunContext) newStepExecutor(step *model.Step) common.Executor {
	sc := &StepContext{
		RunContext: rc,
		Step:       step,
	}
	return func(ctx context.Context) error {
		rc.CurrentStep = sc.Step.ID
		rc.StepResults[rc.CurrentStep] = &stepResult{
			Success: true,
			Outputs: make(map[string]string),
		}

		_ = sc.setupEnv()(ctx)
		rc.ExprEval = sc.NewExpressionEvaluator()

		if !rc.EvalBool(sc.Step.If) {
			log.Debugf("Skipping step '%s' due to '%s'", sc.Step.String(), sc.Step.If)
			return nil
		}

		common.Logger(ctx).Infof("\u2B50  Run %s", sc.Step)
		err := sc.Executor()(ctx)
		if err == nil {
			common.Logger(ctx).Infof("  \u2705  Success - %s", sc.Step)
		} else {
			common.Logger(ctx).Errorf("  \u274C  Failure - %s", sc.Step)
			rc.StepResults[rc.CurrentStep].Success = false
		}
		return err
	}
}

func (rc *RunContext) platformImage() string {
	job := rc.Run.Job()

	c := job.Container()
	if c != nil {
		return c.Image
	}

	for _, runnerLabel := range job.RunsOn() {
		platformName := rc.ExprEval.Interpolate(runnerLabel)
		image := rc.Config.Platforms[strings.ToLower(platformName)]
		if image != "" {
			return image
		}
	}

	return ""
}

func (rc *RunContext) isEnabled(ctx context.Context) bool {
	job := rc.Run.Job()
	l := common.Logger(ctx)
	if !rc.EvalBool(job.If) {
		l.Debugf("Skipping job '%s' due to '%s'", job.Name, job.If)
		return false
	}

	img := rc.platformImage()
	if img == "" {
		l.Infof("\U0001F6A7  Skipping unsupported platform '%+v'", job.RunsOn())
		return false
	}
	return true
}

// EvalBool evaluates an expression against current run context
func (rc *RunContext) EvalBool(expr string) bool {
	if expr != "" {
		expr = fmt.Sprintf("Boolean(%s)", rc.ExprEval.Interpolate(expr))
		v, err := rc.ExprEval.Evaluate(expr)
		if err != nil {
			return false
		}
		log.Debugf("expression '%s' evaluated to '%s'", expr, v)
		return v == "true"
	}
	return true
}

func mergeMaps(maps ...map[string]string) map[string]string {
	rtnMap := make(map[string]string)
	for _, m := range maps {
		for k, v := range m {
			rtnMap[k] = v
		}
	}
	return rtnMap
}

func createContainerName(parts ...string) string {
	name := make([]string, 0)
	pattern := regexp.MustCompile("[^a-zA-Z0-9]")
	partLen := (30 / len(parts)) - 1
	for i, part := range parts {
		if i == len(parts)-1 {
			name = append(name, pattern.ReplaceAllString(part, "-"))
		} else {
			name = append(name, trimToLen(pattern.ReplaceAllString(part, "-"), partLen))
		}
	}
	return strings.Trim(strings.Join(name, "-"), "-")
}

func trimToLen(s string, l int) string {
	if l < 0 {
		l = 0
	}
	if len(s) > l {
		return s[:l]
	}
	return s
}

type jobContext struct {
	Status    string `json:"status"`
	Container struct {
		ID      string `json:"id"`
		Network string `json:"network"`
	} `json:"container"`
	Services map[string]struct {
		ID string `json:"id"`
	} `json:"services"`
}

func (rc *RunContext) getJobContext() *jobContext {
	jobStatus := "success"
	for _, stepStatus := range rc.StepResults {
		if !stepStatus.Success {
			jobStatus = "failure"
			break
		}
	}
	return &jobContext{
		Status: jobStatus,
	}
}

func (rc *RunContext) getStepsContext() map[string]*stepResult {
	return rc.StepResults
}

type githubContext struct {
	Event      map[string]interface{} `json:"event"`
	EventPath  string                 `json:"event_path"`
	Workflow   string                 `json:"workflow"`
	RunID      string                 `json:"run_id"`
	RunNumber  string                 `json:"run_number"`
	Actor      string                 `json:"actor"`
	Repository string                 `json:"repository"`
	EventName  string                 `json:"event_name"`
	Sha        string                 `json:"sha"`
	Ref        string                 `json:"ref"`
	HeadRef    string                 `json:"head_ref"`
	BaseRef    string                 `json:"base_ref"`
	Token      string                 `json:"token"`
	Workspace  string                 `json:"workspace"`
	Action     string                 `json:"action"`
}

func (rc *RunContext) getGithubContext() *githubContext {
	token, ok := rc.Config.Secrets["GITHUB_TOKEN"]
	if !ok {
		token = os.Getenv("GITHUB_TOKEN")
	}

	ghc := &githubContext{
		Event:     make(map[string]interface{}),
		EventPath: "/github/workflow/event.json",
		Workflow:  rc.Run.Workflow.Name,
		RunID:     "1",
		RunNumber: "1",
		Actor:     rc.Config.Actor,
		EventName: rc.Config.EventName,
		Token:     token,
		Workspace: "/github/workspace",
		Action:    rc.CurrentStep,
	}

	// Backwards compatibility for configs that require
	// a default rather than being run as a cmd
	if ghc.Actor == "" {
		ghc.Actor = "nektos/act"
	}

	repoPath := rc.Config.Workdir
	repo, err := common.FindGithubRepo(repoPath)
	if err != nil {
		log.Warningf("unable to get git repo: %v", err)
	} else {
		ghc.Repository = repo
	}

	_, sha, err := common.FindGitRevision(repoPath)
	if err != nil {
		log.Warningf("unable to get git revision: %v", err)
	} else {
		ghc.Sha = sha
	}

	ref, err := common.FindGitRef(repoPath)
	if err != nil {
		log.Warningf("unable to get git ref: %v", err)
	} else {
		log.Debugf("using github ref: %s", ref)
		ghc.Ref = ref
	}
	if rc.EventJSON != "" {
		err = json.Unmarshal([]byte(rc.EventJSON), &ghc.Event)
		if err != nil {
			logrus.Errorf("Unable to Unmarshal event '%s': %v", rc.EventJSON, err)
		}
	}

	if ghc.EventName == "pull_request" {
		ghc.BaseRef = asString(nestedMapLookup(ghc.Event, "pull_request", "base", "ref"))
		ghc.HeadRef = asString(nestedMapLookup(ghc.Event, "pull_request", "head", "ref"))
	}

	return ghc
}

func (ghc *githubContext) isLocalCheckout(step *model.Step) bool {
	if step.Type() != model.StepTypeUsesActionRemote {
		return false
	}
	remoteAction := newRemoteAction(step.Uses)
	if !remoteAction.IsCheckout() {
		return false
	}

	if repository, ok := step.With["repository"]; ok && repository != ghc.Repository {
		return false
	}
	if repository, ok := step.With["ref"]; ok && repository != ghc.Ref {
		return false
	}
	return true
}

func asString(v interface{}) string {
	if v == nil {
		return ""
	} else if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func nestedMapLookup(m map[string]interface{}, ks ...string) (rval interface{}) {
	var ok bool

	if len(ks) == 0 { // degenerate input
		return nil
	}
	if rval, ok = m[ks[0]]; !ok {
		return nil
	} else if len(ks) == 1 { // we've reached the final key
		return rval
	} else if m, ok = rval.(map[string]interface{}); !ok {
		return nil
	} else { // 1+ more keys
		return nestedMapLookup(m, ks[1:]...)
	}
}

func (rc *RunContext) withGithubEnv(env map[string]string) map[string]string {
	github := rc.getGithubContext()
	env["HOME"] = "/github/home"
	env["GITHUB_WORKFLOW"] = github.Workflow
	env["GITHUB_RUN_ID"] = github.RunID
	env["GITHUB_RUN_NUMBER"] = github.RunNumber
	env["GITHUB_ACTION"] = github.Action
	env["GITHUB_ACTIONS"] = "true"
	env["GITHUB_ACTOR"] = github.Actor
	env["GITHUB_REPOSITORY"] = github.Repository
	env["GITHUB_EVENT_NAME"] = github.EventName
	env["GITHUB_EVENT_PATH"] = github.EventPath
	env["GITHUB_WORKSPACE"] = github.Workspace
	env["GITHUB_SHA"] = github.Sha
	env["GITHUB_REF"] = github.Ref
	env["GITHUB_TOKEN"] = github.Token
	return env
}

func (rc *RunContext) localCheckoutPath() (string, bool) {
	ghContext := rc.getGithubContext()
	for _, step := range rc.Run.Job().Steps {
		if ghContext.isLocalCheckout(step) {
			return step.With["path"], true
		}
	}
	return "", false
}
