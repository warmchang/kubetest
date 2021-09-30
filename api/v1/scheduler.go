//go:build !ignore_autogenerated
// +build !ignore_autogenerated

package v1

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type TaskScheduler struct {
	strategy *Strategy
	builder  *TaskBuilder
}

func NewTaskScheduler(strategy *Strategy, builder *TaskBuilder) *TaskScheduler {
	return &TaskScheduler{
		strategy: strategy,
		builder:  builder,
	}
}

type StrategyKey struct {
	ConcurrentIdx    int
	Keys             []string
	Env              string
	SubTaskScheduler *SubTaskScheduler
	OnFinishSubTask  func(*SubTask)
}

func (s *TaskScheduler) Schedule(ctx context.Context, tmpl TestJobTemplateSpec) (*TaskGroup, error) {
	if s.strategy == nil {
		task, err := s.builder.Build(ctx, tmpl)
		if err != nil {
			return nil, err
		}
		return NewTaskGroup([]*Task{task}), nil
	}
	keys, err := s.getScheduleKeys(ctx, s.strategy.Key.Source)
	if err != nil {
		return nil, err
	}
	subTaskScheduler := NewSubTaskScheduler(s.strategy.Scheduler.MaxConcurrentNumPerPod)
	maxContainers := s.strategy.Scheduler.MaxContainersPerPod

	var (
		finishedKeyNum uint32
		keyNum         uint32 = uint32(len(keys))
		onFinishMu     sync.Mutex
	)
	if len(keys) <= maxContainers {
		task, err := s.builder.BuildWithKey(ctx, tmpl, &StrategyKey{
			ConcurrentIdx:    0,
			Keys:             keys,
			SubTaskScheduler: subTaskScheduler,
			Env:              s.strategy.Key.Env,
			OnFinishSubTask: func(_ *SubTask) {
				onFinishMu.Lock()
				defer onFinishMu.Unlock()
				finishedKeyNum++
				LoggerFromContext(ctx).Info(
					"%d/%d (%f%%) finished.",
					finishedKeyNum, keyNum, (float32(finishedKeyNum)/float32(keyNum))*100,
				)
			},
		})
		if err != nil {
			return nil, err
		}
		return NewTaskGroup([]*Task{task}), nil
	}
	concurrent := len(keys) / maxContainers
	tasks := []*Task{}
	sum := 0
	for i := 0; i <= concurrent; i++ {
		var taskKeys []string
		if i == concurrent {
			taskKeys = keys[sum:]
		} else {
			taskKeys = keys[sum : sum+maxContainers]
		}
		task, err := s.builder.BuildWithKey(ctx, tmpl, &StrategyKey{
			ConcurrentIdx:    i,
			Keys:             taskKeys,
			SubTaskScheduler: subTaskScheduler,
			Env:              s.strategy.Key.Env,
			OnFinishSubTask: func(_ *SubTask) {
				atomic.AddUint32(&finishedKeyNum, 1)
				LoggerFromContext(ctx).Info(
					"%d/%d (%f%%) finished.",
					finishedKeyNum, keyNum, (float32(finishedKeyNum)/float32(keyNum))*100,
				)
			},
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
		sum += maxContainers
	}
	return NewTaskGroup(tasks), nil
}

func (s *TaskScheduler) getScheduleKeys(ctx context.Context, source StrategyKeySource) ([]string, error) {
	switch {
	case len(source.Static) > 0:
		LoggerFromContext(ctx).Info(
			"found %d static keys to start distributed task",
			len(source.Static),
		)
		return source.Static, nil
	case source.Dynamic != nil:
		return s.dynamicKeys(ctx, source.Dynamic)
	default:
		return nil, fmt.Errorf("kubetest: invalid schedule key source")
	}
}

func (s *TaskScheduler) dynamicKeys(ctx context.Context, source *StrategyDynamicKeySource) ([]string, error) {
	keyTask, err := s.builder.Build(ctx, source.Spec)
	if err != nil {
		return nil, err
	}
	result, err := keyTask.Run(ctx)
	if err != nil {
		return nil, err
	}
	mainResults := result.MainTaskResults()
	if len(mainResults) == 0 {
		return nil, fmt.Errorf("kubetest: failed to find main task results for dynamic keys")
	}
	if len(mainResults) > 1 {
		return nil, fmt.Errorf("kubetest: found multiple main task results")
	}
	out := mainResults[0].Out
	filter, err := s.sourceFilter(source.Filter)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, key := range strings.Split(string(out), s.sourceDelim(source.Delim)) {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if filter != nil && !filter.MatchString(key) {
			continue
		}
		keys = append(keys, key)
	}
	LoggerFromContext(ctx).Info(
		"found %d dynamic keys to start distributed task. elapsed time %f sec",
		len(keys),
		mainResults[0].ElapsedTime.Seconds(),
	)
	return keys, nil
}

func (s *TaskScheduler) sourceFilter(filter string) (*regexp.Regexp, error) {
	if filter == "" {
		return nil, nil
	}
	return regexp.Compile(filter)
}

func (s *TaskScheduler) sourceDelim(delim string) string {
	const (
		defaultDelim = "\n"
	)
	if delim == "" {
		return defaultDelim
	}
	return delim
}

func NewSubTaskScheduler(maxConcurrentNumPerPod int) *SubTaskScheduler {
	return &SubTaskScheduler{
		maxConcurrentNumPerPod: maxConcurrentNumPerPod,
	}
}

type SubTaskScheduler struct {
	maxConcurrentNumPerPod int
}

func (s *SubTaskScheduler) Schedule(tasks []*SubTask) []*SubTaskGroup {
	concurrentNum := s.getConcurrentNum(len(tasks))
	taskNum := len(tasks)
	groups := []*SubTaskGroup{}
	if concurrentNum > 0 {
		concurrent := concurrentNum
		for i := 0; i < taskNum; i += concurrent {
			start := i
			end := i + concurrent
			if end > taskNum {
				end = taskNum
			}
			groups = append(groups, NewSubTaskGroup(tasks[start:end]))
		}
	} else {
		groups = append(groups, NewSubTaskGroup(tasks))
	}
	return groups
}

func (s *SubTaskScheduler) getConcurrentNum(taskNum int) int {
	maxConcurrentNum := s.maxConcurrentNumPerPod
	if maxConcurrentNum <= 0 {
		return taskNum
	}
	if maxConcurrentNum > taskNum {
		return taskNum
	}
	return maxConcurrentNum
}
