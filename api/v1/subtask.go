//go:build !ignore_autogenerated
// +build !ignore_autogenerated

package v1

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
)

type SubTask struct {
	Name         string
	KeyEnvName   string
	OnFinish     func(*SubTask)
	exec         JobExecutor
	isMain       bool
	copyArtifact func(context.Context, *SubTask) error
}

func (t *SubTask) Run(ctx context.Context) (*SubTaskResult, error) {
	logger := LoggerFromContext(ctx)
	logGroup := logger.Group()
	ctx = WithLogger(ctx, logGroup)
	defer func() {
		if err := t.exec.Stop(ctx); err != nil {
			logGroup.Warn("failed to stop %s", err)
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
	logGroup.Info(result.Command())
	logGroup.Log(string(out))
	if err == nil {
		result.Status = TaskResultSuccess
	} else {
		logGroup.Log(err.Error())
		result.Status = TaskResultFailure
	}
	logGroup.Info("elapsed time: %f sec.", result.ElapsedTime.Seconds())
	if err := t.copyArtifact(ctx, t); err != nil {
		logGroup.Warn("failed to copy artifact: %s", err.Error())
		result.Status = TaskResultFailure
		result.ArtifactErr = err
	}
	return result, nil
}

type SubTaskGroup struct {
	tasks []*SubTask
}

func NewSubTaskGroup(tasks []*SubTask) *SubTaskGroup {
	return &SubTaskGroup{
		tasks: tasks,
	}
}

func (g *SubTaskGroup) Run(ctx context.Context) (*SubTaskResultGroup, error) {
	var (
		eg errgroup.Group
		rg SubTaskResultGroup
	)
	for _, task := range g.tasks {
		task := task
		eg.Go(func() error {
			result, err := task.Run(ctx)
			if err != nil {
				return err
			}
			rg.add(result)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return &rg, nil
}

type TaskResultStatus int

const (
	TaskResultSuccess TaskResultStatus = iota
	TaskResultFailure
)

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
		return fmt.Sprintf("%s=%s; ", envName, r.Name) + cmd
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
