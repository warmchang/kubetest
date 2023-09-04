//go:build !ignore_autogenerated
// +build !ignore_autogenerated

package v1

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/goccy/kubejob"
	corev1 "k8s.io/api/core/v1"
)

type SubTask struct {
	Name         string
	TaskName     string
	KeyEnvName   string
	OnFinish     func(*SubTask)
	exec         JobExecutor
	isMain       bool
	copyArtifact func(context.Context, *SubTask) error
}

func (t *SubTask) outputError(logGroup Logger, baseErr error) {
	if baseErr == nil {
		return
	}
	failedJob, ok := baseErr.(*kubejob.FailedJob)
	if !ok {
		logGroup.Log(baseErr.Error())
		return
	}
	if failedJob.Reason == nil {
		return
	}
	cmdErr, ok := failedJob.Reason.(*kubejob.CommandError)
	if !ok {
		logGroup.Log(failedJob.Reason.Error())
		return
	}
	if !cmdErr.IsExitError() {
		logGroup.Log(cmdErr.Error())
	}
}

const (
	terminationLog = "kubetest task is completed"
)

func (t *SubTask) Run(ctx context.Context) *SubTaskResult {
	logger := LoggerFromContext(ctx)
	logGroup := logger.Group()
	ctx = WithLogger(ctx, logGroup)
	defer func() {
		if err := t.exec.TerminationLog(ctx, terminationLog); err != nil {
			logGroup.Warn("failed to send termination log: %s", err.Error())
		}
		logger.LogGroup(logGroup)
		if t.OnFinish != nil {
			t.OnFinish(t)
		}
	}()
	start := time.Now()
	out, err := t.exec.Output(ctx)
	result := &SubTaskResult{
		ElapsedTime: time.Since(start),
		Out:         out,
		Err:         err,
		Name:        t.Name,
		Container:   t.exec.Container(),
		Pod:         t.exec.Pod(),
		IsMain:      t.isMain,
		KeyEnvName:  t.KeyEnvName,
	}
	logGroup.Debug("container: %s", t.exec.Container().Name)
	logGroup.Log(result.Command())
	logGroup.Log(string(out))
	if err == nil {
		result.Status = TaskResultSuccess
	} else {
		t.outputError(logGroup, err)
		result.Status = TaskResultFailure
	}
	if t.TaskName != "" {
		logGroup.Info("%s: elapsed time: %f sec.", t.TaskName, result.ElapsedTime.Seconds())
	} else {
		logGroup.Info("elapsed time: %f sec.", result.ElapsedTime.Seconds())
	}
	if err := t.copyArtifact(ctx, t); err != nil {
		logGroup.Error("failed to copy artifact: %s", err.Error())
		result.Status = TaskResultFailure
		result.ArtifactErr = err
	}
	return result
}

type SubTaskGroup struct {
	tasks []*SubTask
}

func NewSubTaskGroup(tasks []*SubTask) *SubTaskGroup {
	return &SubTaskGroup{
		tasks: tasks,
	}
}

func (g *SubTaskGroup) Run(ctx context.Context) *SubTaskResultGroup {
	var (
		wg sync.WaitGroup
		rg SubTaskResultGroup
	)
	for _, task := range g.tasks {
		task := task
		wg.Add(1)
		go func() {
			rg.add(task.Run(ctx))
			wg.Done()
		}()
	}
	wg.Wait()
	return &rg
}

type TaskResultStatus int

const (
	TaskResultSuccess TaskResultStatus = iota
	TaskResultFailure
)

func (s TaskResultStatus) ToResultStatus() ResultStatus {
	switch s {
	case TaskResultSuccess:
		return ResultStatusSuccess
	case TaskResultFailure:
		return ResultStatusFailure
	}
	return ResultStatusError
}

func (s TaskResultStatus) String() string {
	switch s {
	case TaskResultSuccess:
		return "success"
	case TaskResultFailure:
		return "failure"
	}
	return "unknown"
}

func (s TaskResultStatus) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`"%s"`, s.String())), nil
}

type SubTaskResult struct {
	Status      TaskResultStatus
	ElapsedTime time.Duration
	Out         []byte
	Err         error
	ArtifactErr error
	Name        string
	Container   corev1.Container
	Pod         *corev1.Pod
	KeyEnvName  string
	IsMain      bool
}

func (r *SubTaskResult) Error() error {
	errs := []string{}
	if r.Err != nil {
		errs = append(errs, r.Err.Error())
	}
	if r.ArtifactErr != nil {
		errs = append(errs, r.ArtifactErr.Error())
	}
	if len(errs) > 0 {
		return fmt.Errorf(strings.Join(errs, ":"))
	}
	return nil
}

func (r *SubTaskResult) Command() string {
	cmd := strings.Join(append(r.Container.Command, r.Container.Args...), " ")
	envName := r.KeyEnvName
	if envName != "" {
		return fmt.Sprintf("[%s:%s] ", envName, r.Name) + cmd
	}
	return cmd
}

type SubTaskResultGroup struct {
	results []*SubTaskResult
	mu      sync.Mutex
}

func (g *SubTaskResultGroup) add(result *SubTaskResult) {
	g.mu.Lock()
	g.results = append(g.results, result)
	g.mu.Unlock()
}
