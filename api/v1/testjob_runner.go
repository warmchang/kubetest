// +build !ignore_autogenerated

package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/kubejob"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	ErrFailedTestJob = xerrors.New("failed test job")
	ErrFatal         = xerrors.New("fatal error")
)

type TestResult string

const (
	// TestResultSuccess represents that all test cases have passed.
	TestResultSuccess TestResult = "success"
	// TestResultFailure represents failed test case exists.
	TestResultFailure TestResult = "failure"
	// TestResultError is unexpected internal error.
	TestResultError TestResult = "error"
)

type TestResultLog struct {
	TestResult     TestResult          `json:"testResult"`
	Job            string              `json:"job"`
	ElapsedTimeSec int                 `json:"elapsedTimeSec"`
	StartedAt      time.Time           `json:"startedAt"`
	Details        TestResultLogDetail `json:"details"`
}

type TestResultLogDetail struct {
	Tests []*TestLog `json:"tests"`
}

type MaskedMessage struct {
	msg   string
	masks []string
	mu    sync.Mutex
}

func newMaskedMessage(msg string, masks []string) *MaskedMessage {
	return &MaskedMessage{msg: msg, masks: masks}
}

func (m *MaskedMessage) addMessage(msg string) {
	m.mu.Lock()
	m.msg += msg
	m.mu.Unlock()
}

func (m *MaskedMessage) addMask(mask string) {
	m.mu.Lock()
	m.masks = append(m.masks, mask)
	m.mu.Unlock()
}

func (m *MaskedMessage) Filter(msg string) string {
	m.mu.Lock()
	masks := m.masks
	m.mu.Unlock()
	return m.mask(msg, masks)
}

func (m *MaskedMessage) mask(msg string, masks []string) string {
	maskedMsg := msg
	for _, mask := range masks {
		genMaskText := strings.Repeat("*", len(mask))
		maskedMsg = strings.Replace(maskedMsg, mask, genMaskText, -1)
	}
	return maskedMsg
}

func (m *MaskedMessage) String() string {
	m.mu.Lock()
	msg := m.msg
	masks := m.masks
	m.mu.Unlock()
	return m.mask(msg, masks)
}

type TestLog struct {
	Name           string         `json:"name"`
	TestResult     TestResult     `json:"testResult"`
	ElapsedTimeSec int            `json:"elapsedTimeSec"`
	Message        *MaskedMessage `json:"-"`
}

type TestJobRunner struct {
	token              string
	disabledPrepareLog bool
	disabledCommandLog bool
	disabledResultLog  bool
	verboseLog         bool
	logger             func(*kubejob.ContainerLog)
	config             *rest.Config
	clientSet          *kubernetes.Clientset
	logPrinter         *Logger
	masks              []string
	testCountMu        sync.Mutex
	testCount          uint
	totalTestNum       uint
	copyMu             sync.Mutex
}

func NewTestJobRunner(config *rest.Config) (*TestJobRunner, error) {
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, xerrors.Errorf("failed to create clientset: %w", err)
	}
	return &TestJobRunner{
		config:     config,
		clientSet:  cs,
		logPrinter: newLogger(),
	}, nil
}

func (r *TestJobRunner) SetToken(token string) {
	r.token = token
}

func (r *TestJobRunner) EnableVerboseLog() {
	r.verboseLog = true
}

func (r *TestJobRunner) DisablePrepareLog() {
	r.disabledPrepareLog = true
}

func (r *TestJobRunner) DisableCommandLog() {
	r.disabledCommandLog = true
}

func (r *TestJobRunner) DisableResultLog() {
	r.disabledResultLog = true
}

func (r *TestJobRunner) SetLogger(logger func(*kubejob.ContainerLog)) {
	r.logger = logger
}

func (r *TestJobRunner) Run(ctx context.Context, testjob TestJob) error {
	if err := testjob.validate(); err != nil {
		return xerrors.Errorf("validate error: %w", err)
	}
	testLog := TestResultLog{Job: testjob.ObjectMeta.Name, StartedAt: time.Now()}

	defer func(start time.Time) {
		if r.disabledResultLog {
			return
		}
		testLog.ElapsedTimeSec = int(time.Since(start).Seconds())
		b, _ := json.Marshal(testLog)

		var logMap map[string]interface{}
		json.Unmarshal(b, &logMap)

		for k, v := range testjob.Spec.Log.ExtParam {
			logMap[k] = v
		}
		b, _ = json.Marshal(logMap)
		fmt.Println(string(b))
	}(time.Now())

	testLogs, err := r.run(ctx, testjob)
	testLog.Details = TestResultLogDetail{
		Tests: testLogs,
	}
	if err != nil {
		if xerrors.Is(err, ErrFatal) {
			testLog.TestResult = TestResultError
		} else {
			testLog.TestResult = TestResultFailure
		}
		return err
	}
	testLog.TestResult = TestResultSuccess
	return nil
}

func (r *TestJobRunner) run(ctx context.Context, testjob TestJob) ([]*TestLog, error) {
	if err := r.setGitToken(ctx, testjob); err != nil {
		return nil, xerrors.Errorf("failed to set git token: %w", err)
	}
	if err := r.prepare(ctx, testjob); err != nil {
		return nil, err
	}
	if testjob.enabledDistributedTest() {
		return r.runDistributedTest(ctx, testjob)
	}
	return r.runTest(ctx, testjob)
}

func (r *TestJobRunner) setGitToken(ctx context.Context, testjob TestJob) error {
	jobToken := testjob.gitToken()
	if jobToken == nil {
		return nil
	}
	secret, err := r.clientSet.CoreV1().
		Secrets(testjob.Namespace).
		Get(ctx, jobToken.SecretKeyRef.Name, metav1.GetOptions{})
	if err != nil {
		return xerrors.Errorf("failed to read secret for git token: %w", err)
	}
	data, exists := secret.Data[jobToken.SecretKeyRef.Key]
	if !exists {
		return xerrors.Errorf("not found token: %s", jobToken.SecretKeyRef.Key)
	}
	r.token = strings.TrimSpace(string(data))
	r.logPrinter.addMask(r.token)
	r.masks = append(r.masks, r.token)
	return nil
}

func (r *TestJobRunner) prepare(ctx context.Context, testjob TestJob) error {
	if !testjob.existsPrepareSteps() {
		return nil
	}
	template, err := testjob.createPrepareJobTemplate(r.token)
	if err != nil {
		return xerrors.Errorf("failed to create prepare job template: %w", err)
	}
	job, err := r.createKubeJob(testjob, template)
	if err != nil {
		return xerrors.Errorf("failed to create kubejob instance for prepare steps: %w", err)
	}
	job.DisableCommandLog()
	if r.logger != nil {
		job.SetContainerLogger(r.logger)
	}

	defer func(start time.Time) {
		fmt.Fprintf(os.Stderr, "prepare: elapsed time %f sec\n", time.Since(start).Seconds())
	}(time.Now())

	if err := job.Run(ctx); err != nil {
		return xerrors.Errorf("failed to run prepare steps: %w", err)
	}
	return nil
}

func (r *TestJobRunner) createKubeJob(testjob TestJob, template apiv1.PodTemplateSpec) (*kubejob.Job, error) {
	job, err := kubejob.NewJobBuilder(r.config, testjob.Namespace).
		BuildWithJob(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: r.generateName(testjob.ObjectMeta.Name),
			},
			Spec: batchv1.JobSpec{
				Template: template,
			},
		})
	if err != nil {
		return nil, xerrors.Errorf("failed to create job: %w", err)
	}
	if r.verboseLog {
		job.SetLogger(func(log string) {
			r.logPrinter.DebugLog(log)
		})
		job.SetVerboseLog(true)
	}
	return job, nil
}

func (r *TestJobRunner) generateName(name string) string {
	return fmt.Sprintf("%s-%s", name, xid.New())
}

func (r *TestJobRunner) runTest(ctx context.Context, testjob TestJob) ([]*TestLog, error) {
	job, err := r.createKubeJob(testjob, testjob.createJobTemplate(r.token))
	if err != nil {
		return nil, err
	}
	if r.logger != nil {
		job.SetContainerLogger(r.logger)
	}
	if r.disabledPrepareLog {
		job.DisableInitContainerLog()
	}
	if r.disabledCommandLog {
		job.DisableCommandLog()
	}
	if err := job.Run(ctx); err != nil {
		var failedJob *kubejob.FailedJob
		if xerrors.As(err, &failedJob) {
			return nil, ErrFailedTestJob
		}
		log.Printf(err.Error())
		return nil, ErrFailedTestJob
	}
	return nil, nil
}

func (r *TestJobRunner) validateTestLogs(tests []string, testlogs []*TestLog) error {
	testLogMap := map[string]struct{}{}
	for _, log := range testlogs {
		testLogMap[log.Name] = struct{}{}
	}
	invalidTests := []string{}
	for _, test := range tests {
		if _, exists := testLogMap[test]; exists {
			continue
		}
		invalidTests = append(invalidTests, test)
	}
	if len(invalidTests) > 0 {
		return xerrors.Errorf("failed to find [ %s ] test logs: %w", strings.Join(invalidTests, ","), ErrFatal)
	}
	return nil
}

func (r *TestJobRunner) runDistributedTest(ctx context.Context, testjob TestJob) ([]*TestLog, error) {
	fmt.Println("get listing of tests...")
	list, err := r.testList(ctx, testjob)
	if err != nil {
		return nil, xerrors.Errorf("failed to get list for testing: %w", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	r.totalTestNum = uint(len(list))

	plan := testjob.plan(list)

	defer func(start time.Time) {
		fmt.Fprintf(os.Stderr, "test: elapsed time %f sec\n", time.Since(start).Seconds())
	}(time.Now())

	testLogs := []*TestLog{}
	testLogMu := sync.Mutex{}

	var eg errgroup.Group
	for _, tests := range plan {
		tests := tests
		eg.Go(func() error {
			logs, err := r.runTests(ctx, testjob, tests)
			if err != nil {
				return xerrors.Errorf("failed to runTests: %w", err)
			}
			testLogMu.Lock()
			testLogs = append(testLogs, logs...)
			testLogMu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, xerrors.Errorf("failed to distributed test job: %w", err)
	}

	if err := r.validateTestLogs(list, testLogs); err != nil {
		return nil, xerrors.Errorf("invalid testlogs: %w", err)
	}

	failedTestLogs := []*TestLog{}
	for _, testLog := range testLogs {
		if testLog.TestResult == TestResultFailure {
			failedTestLogs = append(failedTestLogs, testLog)
		}
	}
	if len(failedTestLogs) > 0 {
		if !testjob.enabledRetest() {
			return testLogs, ErrFailedTestJob
		}
		return r.retest(ctx, testjob, testLogs, failedTestLogs)
	}
	return testLogs, nil
}

func (r *TestJobRunner) retest(ctx context.Context, testjob TestJob, testLogs, failedTestLogs []*TestLog) ([]*TestLog, error) {
	fmt.Println("start retest....")
	tests := []string{}
	for _, log := range failedTestLogs {
		tests = append(tests, log.Name)
	}

	// force sequential running
	testjob.Spec.DistributedTest.MaxConcurrentNumPerPod = 1
	r.totalTestNum = uint(len(tests))
	r.testCount = 0

	retestLogs, err := r.runTests(ctx, testjob, tests)
	retestLogMap := map[string]*TestLog{}
	for _, log := range retestLogs {
		retestLogMap[log.Name] = log
	}
	var existsFailedTest bool
	for idx := range testLogs {
		name := testLogs[idx].Name
		retestLog, exists := retestLogMap[name]
		if !exists {
			continue
		}
		testLogs[idx] = retestLog
		if retestLog.TestResult == TestResultFailure {
			existsFailedTest = true
		}
	}
	if err != nil {
		return testLogs, xerrors.Errorf("%s: %w", err, ErrFailedTestJob)
	}
	if existsFailedTest {
		return testLogs, ErrFailedTestJob
	}
	return testLogs, nil
}

func (r *TestJobRunner) execTests(testjob TestJob, executors []*kubejob.JobExecutor) ([]*TestLog, error) {
	var (
		eg       errgroup.Group
		logMu    sync.Mutex
		testLogs []*TestLog
	)
	for _, executor := range executors {
		executor := executor
		eg.Go(func() error {
			testLog, err := r.execTest(testjob, executor)
			if err != nil {
				return xerrors.Errorf("failed to exec test: %w", err)
			}
			logMu.Lock()
			testLogs = append(testLogs, testLog)
			logMu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, xerrors.Errorf("failed to run tests. first occurred error: %w", err)
	}
	return testLogs, nil
}

func (r *TestJobRunner) execTest(testjob TestJob, executor *kubejob.JobExecutor) (*TestLog, error) {
	testName := testjob.testNameByExecutor(executor)

	defer func() {
		if err := executor.Stop(); err != nil {
			r.logPrinter.DebugLog(fmt.Sprintf("failed to stop %s container", testName))
		}
	}()
	testCommand, err := testjob.testCommand(testName)
	if err != nil {
		return nil, xerrors.Errorf("failed to get test command: %w", err)
	}

	start := time.Now()
	out, err := executor.ExecOnly()
	testCount := r.addTestCount()
	testLog := &TestLog{
		Name:           testName,
		ElapsedTimeSec: int(time.Since(start).Seconds()),
		Message:        newMaskedMessage(string(out), r.masks),
	}

	var testReport string
	if err == nil {
		testLog.TestResult = TestResultSuccess
		testReport = fmt.Sprintf("%s\n%s", testCommand, newMaskedMessage(string(out), r.masks))
	} else {
		testLog.TestResult = TestResultFailure
		testReport = fmt.Sprintf(
			"%s\n%s\n%s\nerror pod: %s container: %s",
			testCommand,
			newMaskedMessage(string(out), r.masks),
			err,
			executor.Pod.Name,
			executor.Container.Name,
		)
	}
	timeReport := fmt.Sprintf("elapsed time: %dsec (current time: %s)", testLog.ElapsedTimeSec, time.Now().Format(time.RFC3339))
	progressReport := fmt.Sprintf("%d/%d (%f%%) finished.", testCount, r.totalTestNum, (float32(testCount)/float32(r.totalTestNum))*100)
	r.logPrinter.Log(strings.Join([]string{testReport, timeReport, progressReport}, "\n") + "\n")

	if err := r.syncArtifactsIfNeeded(testjob, executor, testName); err != nil {
		r.logPrinter.DebugLog(fmt.Sprintf("failed to sync artifacts: %+v", err))
		return nil, xerrors.Errorf("failed to sync artifacts: %w", err)
	}
	return testLog, nil
}

func (r *TestJobRunner) addTestCount() uint {
	r.testCountMu.Lock()
	defer r.testCountMu.Unlock()
	r.testCount++
	return r.testCount
}

func (r *TestJobRunner) runTests(ctx context.Context, testjob TestJob, tests []string) ([]*TestLog, error) {
	template, err := testjob.createTestJobTemplate(r.token, tests)
	if err != nil {
		return nil, xerrors.Errorf("failed to create testjob template: %w", err)
	}
	job, err := r.createKubeJob(testjob, template)
	if err != nil {
		return nil, xerrors.Errorf("failed to create kubejob for test: %w", err)
	}
	initContainersLog := newMaskedMessage("", r.masks)
	job.SetContainerLogger(func(log *kubejob.ContainerLog) {
		if r.isInitContainer(job, log.Container) {
			initContainersLog.addMessage(log.Log)
		}
	})
	job.DisableCommandLog()
	testLogs := []*TestLog{}
	var calledExecutionHandler bool
	if err := job.RunWithExecutionHandler(ctx, func(executors []*kubejob.JobExecutor) error {
		calledExecutionHandler = true
		for _, sidecar := range testjob.filterSidecarExecutors(executors) {
			sidecar.ExecAsync()
		}
		testExecutors := testjob.filterTestExecutors(executors)
		if len(testExecutors) > 0 {
			r.logPrinter.DebugLog(
				fmt.Sprintf(
					"run pod: %s job-id: %s",
					testExecutors[0].Pod.Name,
					testExecutors[0].Pod.Labels[kubejob.KubejobLabel],
				),
			)
		}
		var errs []string
		for _, executors := range testjob.schedule(testExecutors) {
			logs, err := r.execTests(testjob, executors)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%+v", err))
			}
			testLogs = append(testLogs, logs...)
		}
		if len(errs) > 0 {
			return xerrors.Errorf(strings.Join(errs, "\n"))
		}
		return nil
	}); err != nil {
		var failedJob *kubejob.FailedJob
		if !calledExecutionHandler || !xerrors.As(err, &failedJob) {
			initContainersLog := initContainersLog.String()
			return nil, xerrors.Errorf(
				"initContainersLog:[%s]. error detail:[%s]: %w",
				initContainersLog,
				err,
				ErrFailedTestJob,
			)
		}
	}
	return testLogs, nil
}

func (r *TestJobRunner) syncArtifactsIfNeeded(testjob TestJob, executor *kubejob.JobExecutor, testName string) error {
	if testjob.Spec.DistributedTest.Artifacts == nil {
		return nil
	}
	artifacts := testjob.Spec.DistributedTest.Artifacts

	var intermediateDir string
	switch artifacts.Output.PathType {
	case ArtifactOutputPathContainer:
		intermediateDir = executor.Container.Name
	case ArtifactOutputPathTest:
		intermediateDir = testName
	default:
		intermediateDir = executor.Container.Name
	}
	outputDir := filepath.Join(artifacts.Output.Path, intermediateDir)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return xerrors.Errorf("failed to create directory %s: %w", outputDir, err)
	}

	for _, path := range artifacts.Paths {
		var src string
		if filepath.IsAbs(path) {
			src = path
		} else {
			src = filepath.Join(executor.Container.WorkingDir, path)
		}
		r.logPrinter.DebugLog(fmt.Sprintf("copy %s's result file to %s", testName, outputDir))
		if err := r.copyTextFile(executor, src, outputDir); err != nil {
			return xerrors.Errorf("failed to copy %s result from %s to %s: %w", testName, src, outputDir, err)
		}
	}
	return nil
}

func (r *TestJobRunner) createListJob(testjob TestJob) (*kubejob.Job, error) {
	distributedTest := testjob.Spec.DistributedTest
	if distributedTest == nil {
		return nil, xerrors.Errorf("required distributedTest.list param")
	}
	template, err := testjob.createListJobTemplate(r.token)
	if err != nil {
		return nil, xerrors.Errorf("failed to create template for list job: %w", err)
	}
	listjob, err := r.createKubeJob(testjob, template)
	if err != nil {
		return nil, xerrors.Errorf("failed to create list job: %w", err)
	}
	return listjob, nil
}

func (r *TestJobRunner) findExecutorByContainerName(executors []*kubejob.JobExecutor, name string) *kubejob.JobExecutor {
	for _, executor := range executors {
		if executor.Container.Name == name {
			return executor
		}
	}
	return nil
}

func (r *TestJobRunner) isInitContainer(job *kubejob.Job, c apiv1.Container) bool {
	for _, container := range job.Spec.Template.Spec.InitContainers {
		if container.Name == c.Name {
			return true
		}
	}
	return false
}

func (r *TestJobRunner) testList(ctx context.Context, testjob TestJob) ([]string, error) {
	defer func(start time.Time) {
		fmt.Fprintf(os.Stderr, "list: elapsed time %f sec\n", time.Since(start).Seconds())
	}(time.Now())
	names := testjob.listNames()
	if len(names) > 0 {
		return names, nil
	}

	listjob, err := r.createListJob(testjob)
	if err != nil {
		return nil, xerrors.Errorf("failed to create list job: %w", err)
	}
	var (
		initContainersLog = newMaskedMessage("", r.masks)
		containerLog      = newMaskedMessage("", r.masks)
	)
	listjob.SetContainerLogger(func(log *kubejob.ContainerLog) {
		if r.isInitContainer(listjob, log.Container) {
			initContainersLog.addMessage(log.Log)
		} else {
			containerLog.addMessage(log.Log)
		}
	})
	listjob.DisableCommandLog()

	var listResult string
	if err := listjob.RunWithExecutionHandler(ctx, func(executors []*kubejob.JobExecutor) error {
		listExecutor := r.findExecutorByContainerName(executors, listContainerName)
		if listExecutor == nil {
			return xerrors.Errorf("failed to find list container")
		}
		for _, executor := range executors {
			if executor == listExecutor {
				continue
			}
			// sidecar executor
			executor.ExecAsync()
		}
		out, err := listExecutor.Exec()
		listResult = string(out)
		if err != nil {
			return xerrors.Errorf("failed to list command: %w", err)
		}
		return nil
	}); err != nil {
		return nil, xerrors.Errorf(
			"initContainersLog:[%s]. commandLog:[%s] error detail:[%s]: %w",
			initContainersLog.String(),
			listResult,
			err,
			ErrFailedTestJob,
		)
	}
	tests, err := testjob.splitTest(listResult)
	if err != nil {
		return nil, xerrors.Errorf("failed to split test from %s: %w", listResult, err)
	}
	return tests, nil
}
