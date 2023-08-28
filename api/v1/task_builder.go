//go:build !ignore_autogenerated
// +build !ignore_autogenerated

package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
)

const (
	kubetestLabel  = "kubetest.io/testjob"
	keysAnnotation = "kubetest.io/strategyKeys"
)

var (
	logMountPath        = filepath.Join("/", "tmp", "log")
	logMountFilePath    = filepath.Join(logMountPath, "kubetest.log")
	reportMountPath     = filepath.Join("/", "tmp", "report")
	reportMountFilePath = filepath.Join(reportMountPath, "report")
)

type TaskBuilder struct {
	cfg       *rest.Config
	mgr       *ResourceManager
	namespace string
	runMode   RunMode
}

func NewTaskBuilder(cfg *rest.Config, mgr *ResourceManager, namespace string, runMode RunMode) *TaskBuilder {
	return &TaskBuilder{
		cfg:       cfg,
		mgr:       mgr,
		namespace: namespace,
		runMode:   runMode,
	}
}

func (b *TaskBuilder) Build(ctx context.Context, step Step) (*Task, error) {
	return b.BuildWithKey(ctx, step, nil)
}

func (b *TaskBuilder) BuildWithKey(ctx context.Context, step Step, strategyKey *StrategyKey) (*Task, error) {
	tmpl := step.GetTemplate()
	mainContainer, err := getMainContainerFromTmpl(tmpl)
	if err != nil {
		return nil, err
	}
	if mainContainer.Name == "" {
		return nil, fmt.Errorf("kubetest: main container name must be specified")
	}
	createJob := func(ctx context.Context) (Job, error) {
		return b.buildJob(ctx, mainContainer, tmpl, strategyKey)
	}
	job, err := createJob(ctx)
	if err != nil {
		return nil, err
	}
	spec := tmpl.Spec
	artifactMap := map[string][]ArtifactSpec{}
	for _, artifact := range spec.Artifacts {
		artifactMap[artifact.Container.Name] = append(artifactMap[artifact.Container.Name], artifact)
	}
	b.mgr.artifactMgr.AddArtifacts(spec.Artifacts)
	copyArtifact := func(ctx context.Context, subtask *SubTask) error {
		if b.runMode == RunModeDryRun {
			return nil
		}
		var containerName string
		if subtask.isMain {
			containerName = mainContainer.Name
		} else {
			containerName = subtask.exec.Container().Name
		}
		artifacts, exists := artifactMap[containerName]
		if !exists {
			return nil
		}
		for _, artifact := range artifacts {
			localPath, err := b.mgr.ArtifactPathByNameAndContainerName(artifact.Name, subtask.exec.Container().Name)
			if err != nil {
				return err
			}
			if mainContainer.Agent != nil {
				// artifact.Container.Path and localPath has same Base name.
				// If enabled kubetest-agent, try to copy artifacts via normal copy method.
				// So, trim last path.
				localPath = filepath.Dir(localPath)
			}
			if err := subtask.exec.CopyFrom(
				ctx,
				artifact.Container.Path,
				localPath,
			); err != nil {
				return err
			}
		}
		return nil
	}
	var onFinishSubTask func(*SubTask)
	if strategyKey != nil {
		onFinishSubTask = strategyKey.OnFinishSubTask
	}
	return &Task{
		Name:              step.GetName(),
		OnFinishSubTask:   onFinishSubTask,
		job:               job,
		copyArtifact:      copyArtifact,
		strategyKey:       strategyKey,
		mainContainerName: mainContainer.Name,
		createJob:         createJob,
	}, nil
}

func (b *TaskBuilder) buildJob(ctx context.Context, mainContainer TestJobContainer, tmpl TestJobTemplateSpec, strategyKey *StrategyKey) (Job, error) {
	spec := *tmpl.Spec.DeepCopy()
	b.addContainersByStrategyKey(&spec, mainContainer, strategyKey)
	buildCtx := &TaskBuildContext{
		initContainers:      newTaskContainerGroup(spec.InitContainers, spec.Volumes),
		containers:          newTaskContainerGroup(spec.Containers, spec.Volumes),
		finalizerContainers: newTaskContainerGroup([]TestJobContainer{spec.FinalizerContainer}, spec.Volumes),
		spec:                spec,
	}
	podSpec := buildCtx.podSpec()
	podMeta := tmpl.ObjectMeta
	labels := map[string]string{}
	for k, v := range podMeta.Labels {
		labels[k] = v
	}
	labels[kubetestLabel] = fmt.Sprint(true)
	annotations := map[string]string{}
	for k, v := range podMeta.Annotations {
		annotations[k] = v
	}
	if strategyKey != nil {
		keys, err := json.Marshal(strategyKey.Keys)
		if err != nil {
			return nil, fmt.Errorf("kubetest: failed to encode strategy keys: %w", err)
		}
		annotations[keysAnnotation] = string(keys)
	}
	podMeta.Labels = labels
	podMeta.Annotations = annotations
	jobBuilder := NewJobBuilder(b.cfg, b.namespace, b.runMode)
	if spec.FinalizerContainer.Name != "" {
		jobBuilder.SetFinalizer(&spec.FinalizerContainer.Container)
	}
	job, err := jobBuilder.BuildWithJob(&batchv1.Job{
		ObjectMeta: tmpl.ObjectMeta,
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: podMeta,
				Spec:       podSpec,
			},
		},
	}, buildCtx.containerNameToInstalledAgentPathMap(), mainContainer.Agent)
	if err != nil {
		return nil, err
	}
	if buildCtx.needsToPreInit() {
		callback, err := b.preInitCallback(ctx, buildCtx)
		if err != nil {
			return nil, err
		}
		job.PreInit(b.preInitContainer(buildCtx), callback)
	}
	job.Mount(func(ctx context.Context, exec JobExecutor, isInitContainer bool) error {
		containerName := exec.Container().Name
		taskContainer := buildCtx.taskContainer(containerName, isInitContainer)
		if err := b.mountRepository(ctx, taskContainer, exec); err != nil {
			return err
		}
		if err := b.mountToken(ctx, taskContainer, exec); err != nil {
			return err
		}
		if err := b.mountArtifact(ctx, taskContainer, exec); err != nil {
			return err
		}
		if err := b.mountLog(ctx, taskContainer, exec); err != nil {
			return err
		}
		if err := b.mountReport(ctx, taskContainer, exec); err != nil {
			return err
		}
		return nil
	})
	return job, nil
}

func (b *TaskBuilder) mountRepository(ctx context.Context, taskContainer *TaskContainer, exec JobExecutor) error {
	containerName := exec.Container().Name
	LoggerFromContext(ctx).Debug("mount repositories: %s", containerName)
	for repoName, archiveMountPath := range taskContainer.repoNameToArchiveMountPath {
		orgMountPath, exists := taskContainer.repoNameToOrgMountPath[repoName]
		if !exists {
			return fmt.Errorf("kubetest: failed to find org mount path by %s", repoName)
		}
		cmd := []string{
			// remove the mount point path if it already exists.
			"rm", "-rf", orgMountPath,
			"&&",
			// create empty mount point directory.
			"mkdir", "-p", orgMountPath,
			"&&",
			// extract the repository files under the mount point directory.
			"tar", "-zxvf", filepath.Join(archiveMountPath, "repo.tar.gz"), "-C", orgMountPath,
		}
		LoggerFromContext(ctx).Debug(
			"mount repository %s on %s by '%s'",
			containerName, repoName, strings.Join(cmd, " "),
		)
		out, err := exec.PrepareCommand(cmd)
		if err != nil {
			return fmt.Errorf("kubetest: failed to mount repository. %s: %w", string(out), err)
		}
	}
	return nil
}

func (b *TaskBuilder) mountToken(ctx context.Context, taskContainer *TaskContainer, exec JobExecutor) error {
	containerName := exec.Container().Name
	LoggerFromContext(ctx).Debug("mount tokens: %s", containerName)
	for tokenName, mountPath := range taskContainer.tokenNameToMountPath {
		orgMountPath, exists := taskContainer.tokenNameToOrgMountPath[tokenName]
		if !exists {
			return fmt.Errorf("kubetest: failed to find org mount path by %s", tokenName)
		}
		cmd := []string{
			// create mount point base directory if it doesn't exist.
			"mkdir", "-p", filepath.Dir(orgMountPath),
			"&&",
			// copy token file to the mount point path.
			"cp", filepath.Join(mountPath, "token"), orgMountPath,
		}
		LoggerFromContext(ctx).Debug(
			"mount token %s on %s by '%s'",
			containerName, tokenName, strings.Join(cmd, " "),
		)
		out, err := exec.PrepareCommand(cmd)
		if err != nil {
			return fmt.Errorf("kubetest: failed to mount token. %s: %w", string(out), err)
		}
	}
	return nil
}

func (b *TaskBuilder) mountArtifact(ctx context.Context, taskContainer *TaskContainer, exec JobExecutor) error {
	containerName := exec.Container().Name
	LoggerFromContext(ctx).Debug("mount artifacts: %s", containerName)
	if b.runMode == RunModeDryRun {
		return nil
	}
	for artifactName, mountPath := range taskContainer.artifactNameToMountPath {
		orgMountPath, exists := taskContainer.artifactNameToOrgMountPath[artifactName]
		if !exists {
			return fmt.Errorf("kubetest: failed to find org mount path by %s", artifactName)
		}
		localArtifactPath, err := b.mgr.ArtifactPathByName(ctx, artifactName)
		if err != nil {
			return err
		}
		fileName := filepath.Base(localArtifactPath)
		cmd := []string{
			// create base directory for the mount point path.
			"mkdir", "-p", filepath.Dir(orgMountPath),
			"&&",
			// remove the mount point path if it already exists.
			"rm", "-rf", orgMountPath,
			"&&",
			// copy artifacts to the mount point path.
			"cp", "-rf", filepath.Join(mountPath, fileName), orgMountPath,
		}
		LoggerFromContext(ctx).Debug(
			"mount artifact %s on %s by '%s'",
			containerName, artifactName, strings.Join(cmd, " "),
		)
		out, err := exec.PrepareCommand(cmd)
		if err != nil {
			return fmt.Errorf("kubetest: failed to mount artifact. %s: %w", string(out), err)
		}
	}
	return nil
}

func (b *TaskBuilder) mountLog(ctx context.Context, taskContainer *TaskContainer, exec JobExecutor) error {
	containerName := exec.Container().Name
	LoggerFromContext(ctx).Debug("mount log: %s", containerName)
	for _, mountPath := range taskContainer.logOrgMountPaths {
		cmd := []string{
			// create mount point base directory if it doesn't exist.
			"mkdir", "-p", filepath.Dir(mountPath),
			"&&",
			// copy log file to the mount point path.
			"cp", logMountFilePath, mountPath,
		}
		LoggerFromContext(ctx).Debug(
			"mount log on %s by '%s'",
			containerName, strings.Join(cmd, " "),
		)
		out, err := exec.PrepareCommand(cmd)
		if err != nil {
			return fmt.Errorf("kubetest: failed to mount log. %s: %w", string(out), err)
		}
	}
	return nil
}

func (b *TaskBuilder) mountReport(ctx context.Context, taskContainer *TaskContainer, exec JobExecutor) error {
	containerName := exec.Container().Name
	LoggerFromContext(ctx).Debug("mount report: %s", containerName)
	for _, mountPath := range taskContainer.reportOrgMountPaths {
		cmd := []string{
			// create mount point base directory if it doesn't exist.
			"mkdir", "-p", filepath.Dir(mountPath),
			"&&",
			// copy report file to the mount point path.
			"cp", filepath.Join(reportMountPath, "report.json"), mountPath,
		}
		LoggerFromContext(ctx).Debug(
			"mount report on %s by '%s'",
			containerName, strings.Join(cmd, " "),
		)
		out, err := exec.PrepareCommand(cmd)
		if err != nil {
			return fmt.Errorf("kubetest: failed to mount report. %s: %w", string(out), err)
		}
	}
	return nil
}

func (b *TaskBuilder) addContainersByStrategyKey(podSpec *TestJobPodSpec, mainContainer TestJobContainer, strategyKey *StrategyKey) {
	if strategyKey == nil {
		return
	}
	containers := []TestJobContainer{}
	for idx, key := range strategyKey.Keys {
		container := *mainContainer.DeepCopy()
		container.Name += fmt.Sprintf("%d-%d", strategyKey.ConcurrentIdx, idx)
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  strategyKey.Env,
			Value: key,
		})
		containers = append(containers, container)
	}
	sideCarContainers := []TestJobContainer{}
	for _, container := range podSpec.Containers {
		if container.Name == mainContainer.Name {
			continue
		}
		sideCarContainers = append(sideCarContainers, container)
	}
	podSpec.Containers = append(sideCarContainers, containers...)
}

func (b *TaskBuilder) preInitContainer(buildCtx *TaskBuildContext) TestJobContainer {
	return TestJobContainer{
		Container: corev1.Container{
			Name:            "preinit",
			Image:           buildCtx.preInitImage(),
			Command:         []string{"echo"},
			Args:            []string{"-n", "preinit"},
			VolumeMounts:    buildCtx.preInitVolumeMounts(),
			ImagePullPolicy: buildCtx.preInitImagePullPolicy(),
		},
	}
}

func (b *TaskBuilder) preInitCallback(ctx context.Context, buildCtx *TaskBuildContext) (PreInitCallback, error) {
	var defaultCopyTimeout = 10 * time.Minute

	type copyPath struct {
		src string
		dst string
	}

	copyPaths := []*copyPath{}
	if err := b.getCopyPathForRepository(buildCtx, func(src, dst string) {
		copyPaths = append(copyPaths, &copyPath{src: src, dst: dst})
	}); err != nil {
		return nil, err
	}
	if err := b.getCopyPathForToken(ctx, buildCtx, func(src, dst string) {
		copyPaths = append(copyPaths, &copyPath{src: src, dst: dst})
	}); err != nil {
		return nil, err
	}
	if err := b.getCopyPathForArtifact(ctx, buildCtx, func(src, dst string) {
		copyPaths = append(copyPaths, &copyPath{src: src, dst: dst})
	}); err != nil {
		return nil, err
	}
	if err := b.getCopyPathForLog(ctx, buildCtx, func(src, dst string) {
		copyPaths = append(copyPaths, &copyPath{src: src, dst: dst})
	}); err != nil {
		return nil, err
	}
	if err := b.getCopyPathForReport(ctx, buildCtx, func(src, dst string) {
		copyPaths = append(copyPaths, &copyPath{src: src, dst: dst})
	}); err != nil {
		return nil, err
	}
	return func(ctx context.Context, exec JobExecutor) error {
		for _, path := range copyPaths {
			path := path
			if err := func(path *copyPath) error {
				ctx, timeout := context.WithTimeout(ctx, defaultCopyTimeout)
				defer timeout()
				errChan := make(chan error)
				go func() {
					errChan <- exec.CopyTo(ctx, path.src, path.dst)
				}()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case err := <-errChan:
					return err
				}
				return nil
			}(path); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

func (b *TaskBuilder) getCopyPathForRepository(buildCtx *TaskBuildContext, cb func(src, dst string)) error {
	for _, name := range buildCtx.repoNames() {
		src, err := b.mgr.RepositoryPathByName(name)
		if err != nil {
			return err
		}
		dst := buildCtx.repoNameToArchiveMountPath(name)
		cb(src, filepath.Join(dst, filepath.Base(src)))
	}
	return nil
}

func (b *TaskBuilder) getCopyPathForToken(ctx context.Context, buildCtx *TaskBuildContext, cb func(src, dst string)) error {
	for _, name := range buildCtx.tokenNames() {
		src, err := b.mgr.TokenPathByName(ctx, name)
		if err != nil {
			return err
		}
		dst := buildCtx.tokenNameToMountPath(name)
		cb(src, filepath.Join(dst, filepath.Base(src)))
	}
	return nil
}

func (b *TaskBuilder) getCopyPathForArtifact(ctx context.Context, buildCtx *TaskBuildContext, cb func(src, dst string)) error {
	if b.runMode == RunModeDryRun {
		return nil
	}
	for _, name := range buildCtx.artifactNames() {
		src, err := b.mgr.ArtifactPathByName(ctx, name)
		if err != nil {
			return err
		}
		dst := buildCtx.artifactNameToMountPath(name)
		cb(src, dst)
	}
	return nil
}

func (b *TaskBuilder) getCopyPathForLog(ctx context.Context, buildCtx *TaskBuildContext, cb func(src, dst string)) error {
	if b.runMode == RunModeDryRun {
		return nil
	}
	if buildCtx.isUsedLogVolume() {
		logPath, err := b.mgr.LogPath()
		if err != nil {
			return err
		}
		cb(logPath, logMountFilePath)
	}
	return nil
}

func (b *TaskBuilder) getCopyPathForReport(ctx context.Context, buildCtx *TaskBuildContext, cb func(src, dst string)) error {
	if b.runMode == RunModeDryRun {
		return nil
	}
	if buildCtx.isUsedReportVolume() {
		reportPath, err := b.mgr.ReportPath(ReportFormatTypeJSON)
		if err != nil {
			return err
		}
		cb(reportPath, filepath.Join(reportMountPath, filepath.Base(reportPath)))
	}
	return nil
}

type TaskBuildContext struct {
	initContainers      *TaskContainerGroup
	containers          *TaskContainerGroup
	finalizerContainers *TaskContainerGroup
	spec                TestJobPodSpec
}

func (c *TaskBuildContext) taskContainer(name string, isInitContainer bool) *TaskContainer {
	if isInitContainer {
		return c.initContainers.containerMap[name]
	}
	if task := c.finalizerContainers.containerMap[name]; task != nil {
		return task
	}
	return c.containers.containerMap[name]
}

func (c *TaskBuildContext) containerNameToInstalledAgentPathMap() map[string]string {
	ret := c.initContainers.containerNameToInstalledAgentPathMap()
	for k, v := range c.containers.containerNameToInstalledAgentPathMap() {
		ret[k] = v
	}
	for k, v := range c.finalizerContainers.containerNameToInstalledAgentPathMap() {
		ret[k] = v
	}
	path := c.preInitAgentPath()
	if path != "" {
		ret["preinit"] = path
	}
	return ret
}

func (c *TaskBuildContext) isUsedLogVolume() bool {
	for _, container := range c.initContainers.containerMap {
		if len(container.logOrgMountPaths) != 0 {
			return true
		}
	}
	for _, container := range c.containers.containerMap {
		if len(container.logOrgMountPaths) != 0 {
			return true
		}
	}
	for _, container := range c.finalizerContainers.containerMap {
		if len(container.logOrgMountPaths) != 0 {
			return true
		}
	}
	return false
}

func (c *TaskBuildContext) isUsedReportVolume() bool {
	for _, container := range c.initContainers.containerMap {
		if len(container.reportOrgMountPaths) != 0 {
			return true
		}
	}
	for _, container := range c.containers.containerMap {
		if len(container.reportOrgMountPaths) != 0 {
			return true
		}
	}
	for _, container := range c.finalizerContainers.containerMap {
		if len(container.reportOrgMountPaths) != 0 {
			return true
		}
	}
	return false
}

func (c *TaskBuildContext) repoNames() []string {
	repoNameMap := map[string]struct{}{}
	for _, name := range c.initContainers.repoNames() {
		repoNameMap[name] = struct{}{}
	}
	for _, name := range c.containers.repoNames() {
		repoNameMap[name] = struct{}{}
	}
	for _, name := range c.finalizerContainers.repoNames() {
		repoNameMap[name] = struct{}{}
	}
	repoNames := make([]string, 0, len(repoNameMap))
	for name := range repoNameMap {
		repoNames = append(repoNames, name)
	}
	sort.Strings(repoNames)
	return repoNames
}

func (c *TaskBuildContext) tokenNames() []string {
	tokenNameMap := map[string]struct{}{}
	for _, name := range c.initContainers.tokenNames() {
		tokenNameMap[name] = struct{}{}
	}
	for _, name := range c.containers.tokenNames() {
		tokenNameMap[name] = struct{}{}
	}
	for _, name := range c.finalizerContainers.tokenNames() {
		tokenNameMap[name] = struct{}{}
	}
	tokenNames := make([]string, 0, len(tokenNameMap))
	for name := range tokenNameMap {
		tokenNames = append(tokenNames, name)
	}
	sort.Strings(tokenNames)
	return tokenNames
}

func (c *TaskBuildContext) artifactNames() []string {
	artifactNameMap := map[string]struct{}{}
	for _, name := range c.initContainers.artifactNames() {
		artifactNameMap[name] = struct{}{}
	}
	for _, name := range c.containers.artifactNames() {
		artifactNameMap[name] = struct{}{}
	}
	for _, name := range c.finalizerContainers.artifactNames() {
		artifactNameMap[name] = struct{}{}
	}
	artifactNames := make([]string, 0, len(artifactNameMap))
	for name := range artifactNameMap {
		artifactNames = append(artifactNames, name)
	}
	sort.Strings(artifactNames)
	return artifactNames
}

func (g *TaskBuildContext) repoNameToArchiveMountPath(name string) string {
	path := g.initContainers.repoNameToArchiveMountPath(name)
	if path != "" {
		return path
	}
	path = g.containers.repoNameToArchiveMountPath(name)
	if path != "" {
		return path
	}
	return g.finalizerContainers.repoNameToArchiveMountPath(name)
}

func (g *TaskBuildContext) tokenNameToMountPath(name string) string {
	path := g.initContainers.tokenNameToMountPath(name)
	if path != "" {
		return path
	}
	path = g.containers.tokenNameToMountPath(name)
	if path != "" {
		return path
	}
	return g.finalizerContainers.tokenNameToMountPath(name)
}

func (g *TaskBuildContext) artifactNameToMountPath(name string) string {
	path := g.initContainers.artifactNameToMountPath(name)
	if path != "" {
		return path
	}
	path = g.containers.artifactNameToMountPath(name)
	if path != "" {
		return path
	}
	return g.finalizerContainers.artifactNameToMountPath(name)
}

func (c *TaskBuildContext) needsToPreInit() bool {
	return c.initContainers.hasTestVolumeMount() || c.containers.hasTestVolumeMount() || c.finalizerContainers.hasTestVolumeMount()
}

func (c *TaskBuildContext) podSpec() corev1.PodSpec {
	podSpec := c.spec.PodSpec
	initContainers := make([]corev1.Container, 0, len(c.spec.InitContainers))
	for _, container := range c.spec.InitContainers {
		initContainers = append(initContainers, container.Container)
	}
	containers := make([]corev1.Container, 0, len(c.spec.Containers))
	for _, container := range c.spec.Containers {
		containers = append(containers, container.Container)
	}
	podSpec.InitContainers = initContainers
	podSpec.Containers = containers

	podSpecVolumeMap := map[string]corev1.Volume{}
	for k, v := range c.initContainers.podSpecVolumeMap() {
		podSpecVolumeMap[k] = v
	}
	for k, v := range c.containers.podSpecVolumeMap() {
		podSpecVolumeMap[k] = v
	}
	for _, v := range podSpecVolumeMap {
		podSpec.Volumes = append(podSpec.Volumes, v)
	}
	return podSpec
}

func (c *TaskBuildContext) preInitVolumeMounts() []corev1.VolumeMount {
	preInitVolumeMounts := []corev1.VolumeMount{}
	preInitVolumeMountMap := map[string]corev1.VolumeMount{}
	for k, v := range c.initContainers.preInitVolumeMountMap() {
		preInitVolumeMountMap[k] = v
	}
	for k, v := range c.containers.preInitVolumeMountMap() {
		preInitVolumeMountMap[k] = v
	}
	for k, v := range c.finalizerContainers.preInitVolumeMountMap() {
		preInitVolumeMountMap[k] = v
	}
	for _, v := range preInitVolumeMountMap {
		preInitVolumeMounts = append(preInitVolumeMounts, v)
	}
	return preInitVolumeMounts
}

func (c *TaskBuildContext) preInitImage() string {
	image := c.initContainers.preInitImage()
	if image != "" {
		return image
	}
	return c.containers.preInitImage()
}

func (c *TaskBuildContext) preInitImagePullPolicy() corev1.PullPolicy {
	policy := c.initContainers.preInitImagePullPolicy()
	if policy != "" {
		return policy
	}
	return c.containers.preInitImagePullPolicy()
}

func (c *TaskBuildContext) preInitAgentPath() string {
	path := c.initContainers.preInitAgentPath()
	if path != "" {
		return path
	}
	return c.containers.preInitAgentPath()
}

type TaskContainerGroup struct {
	containerMap map[string]*TaskContainer
}

func (g *TaskContainerGroup) containerNameToInstalledAgentPathMap() map[string]string {
	ret := map[string]string{}
	for _, c := range g.containerMap {
		if c.container.Agent != nil {
			ret[c.container.Name] = c.container.Agent.InstalledPath
		}
	}
	return ret
}

func (g *TaskContainerGroup) repoNames() []string {
	repoNameMap := map[string]struct{}{}
	for _, c := range g.containerMap {
		for name := range c.repoNameToArchiveMountPath {
			repoNameMap[name] = struct{}{}
		}
	}
	repoNames := make([]string, 0, len(repoNameMap))
	for name := range repoNameMap {
		repoNames = append(repoNames, name)
	}
	sort.Strings(repoNames)
	return repoNames
}

func (g *TaskContainerGroup) tokenNames() []string {
	tokenNameMap := map[string]struct{}{}
	for _, c := range g.containerMap {
		for name := range c.tokenNameToMountPath {
			tokenNameMap[name] = struct{}{}
		}
	}
	tokenNames := make([]string, 0, len(tokenNameMap))
	for name := range tokenNameMap {
		tokenNames = append(tokenNames, name)
	}
	sort.Strings(tokenNames)
	return tokenNames
}

func (g *TaskContainerGroup) artifactNames() []string {
	artifactNameMap := map[string]struct{}{}
	for _, c := range g.containerMap {
		for name := range c.artifactNameToMountPath {
			artifactNameMap[name] = struct{}{}
		}
	}
	artifactNames := make([]string, 0, len(artifactNameMap))
	for name := range artifactNameMap {
		artifactNames = append(artifactNames, name)
	}
	sort.Strings(artifactNames)
	return artifactNames
}

func (g *TaskContainerGroup) repoNameToArchiveMountPath(name string) string {
	for _, c := range g.containerMap {
		path, exists := c.repoNameToArchiveMountPath[name]
		if exists {
			return path
		}
	}
	return ""
}

func (g *TaskContainerGroup) tokenNameToMountPath(name string) string {
	for _, c := range g.containerMap {
		path, exists := c.tokenNameToMountPath[name]
		if exists {
			return path
		}
	}
	return ""
}

func (g *TaskContainerGroup) artifactNameToMountPath(name string) string {
	for _, c := range g.containerMap {
		path, exists := c.artifactNameToMountPath[name]
		if exists {
			return path
		}
	}
	return ""
}

func (g *TaskContainerGroup) podSpecVolumeMap() map[string]corev1.Volume {
	podSpecVolumeMap := map[string]corev1.Volume{}
	for _, c := range g.containerMap {
		for k, v := range c.podSpecVolumeMap {
			podSpecVolumeMap[k] = v
		}
	}
	return podSpecVolumeMap
}

func (g *TaskContainerGroup) preInitVolumeMountMap() map[string]corev1.VolumeMount {
	preInitVolumeMountMap := map[string]corev1.VolumeMount{}
	for _, c := range g.containerMap {
		for k, v := range c.preInitVolumeMountMap {
			preInitVolumeMountMap[k] = v
		}
	}
	return preInitVolumeMountMap
}

func (g *TaskContainerGroup) preInitAgentPath() string {
	for _, c := range g.containerMap {
		if c.hasTestVolumeMount() && c.container.Agent != nil {
			return c.container.Agent.InstalledPath
		}
	}
	return ""
}

func (g *TaskContainerGroup) hasTestVolumeMount() bool {
	for _, c := range g.containerMap {
		if c.hasTestVolumeMount() {
			return true
		}
	}
	return false
}

func (g *TaskContainerGroup) preInitImage() string {
	for _, c := range g.containerMap {
		if c.hasTestVolumeMount() {
			return c.container.Image
		}
	}
	return ""
}

func (g *TaskContainerGroup) preInitImagePullPolicy() corev1.PullPolicy {
	for _, c := range g.containerMap {
		if c.hasTestVolumeMount() {
			return c.container.ImagePullPolicy
		}
	}
	return ""
}

func newTaskContainerGroup(containers []TestJobContainer, volumes []TestJobVolume) *TaskContainerGroup {
	g := &TaskContainerGroup{
		containerMap: map[string]*TaskContainer{},
	}
	for _, c := range containers {
		g.containerMap[c.Name] = newTaskContainer(c, volumes)
	}
	return g
}

type TaskContainer struct {
	idx                        int
	container                  TestJobContainer
	repoNameToArchiveMountPath map[string]string
	repoNameToOrgMountPath     map[string]string
	tokenNameToMountPath       map[string]string
	tokenNameToOrgMountPath    map[string]string
	artifactNameToMountPath    map[string]string
	artifactNameToOrgMountPath map[string]string
	logOrgMountPaths           []string
	reportOrgMountPaths        []string
	podSpecVolumeMap           map[string]corev1.Volume
	preInitVolumeMountMap      map[string]corev1.VolumeMount
}

func (c *TaskContainer) hasTestVolumeMount() bool {
	return len(c.preInitVolumeMountMap) > 0
}

func newTaskContainer(c TestJobContainer, volumes []TestJobVolume) *TaskContainer {
	repoNameToArchiveMountPath := map[string]string{}
	repoNameToOrgMountPath := map[string]string{}

	tokenNameToMountPath := map[string]string{}
	tokenNameToOrgMountPath := map[string]string{}

	artifactNameToMountPath := map[string]string{}
	artifactNameToOrgMountPath := map[string]string{}

	logOrgMountPaths := []string{}
	reportOrgMountPaths := []string{}

	podSpecVolumeMap := map[string]corev1.Volume{}
	preInitVolumeMountMap := map[string]corev1.VolumeMount{}

	volumeNameToVolume := map[string]TestJobVolume{}
	for _, volume := range volumes {
		volumeNameToVolume[volume.Name] = volume
	}
	for idx, vm := range c.VolumeMounts {
		volume := volumeNameToVolume[vm.Name]
		switch {
		case volume.Repo != nil:
			repoVolumeName := volume.Name
			repoName := volume.Repo.Name
			archiveMountPath := filepath.Join("/", "tmp", "repo-archive", repoVolumeName)
			repoNameToArchiveMountPath[repoName] = archiveMountPath
			repoNameToOrgMountPath[repoName] = vm.MountPath
			c.VolumeMounts[idx].MountPath = archiveMountPath
			// repository archive file mounted to /tmp/repo-archive/name directory on container by emptyDir
			podSpecVolumeMap[repoVolumeName] = corev1.Volume{
				Name: repoVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}
			preInitVolumeMountMap[repoVolumeName] = corev1.VolumeMount{
				Name:      repoVolumeName,
				MountPath: archiveMountPath,
			}
		case volume.Artifact != nil:
			artifactVolumeName := volume.Name
			artifactName := volume.Artifact.Name
			archiveMountPath := filepath.Join("/", "tmp", "artifact-archive", artifactVolumeName)
			artifactNameToMountPath[artifactName] = archiveMountPath
			artifactNameToOrgMountPath[artifactName] = vm.MountPath
			c.VolumeMounts[idx].MountPath = archiveMountPath
			podSpecVolumeMap[artifactVolumeName] = corev1.Volume{
				Name: artifactVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}
			preInitVolumeMountMap[artifactVolumeName] = corev1.VolumeMount{
				Name:      artifactVolumeName,
				MountPath: archiveMountPath,
			}
		case volume.Token != nil:
			tokenVolumeName := volume.Name
			tokenName := volume.Token.Name
			tokenMountPath := filepath.Join("/", "tmp", "token", tokenVolumeName)
			tokenNameToMountPath[tokenName] = tokenMountPath
			tokenNameToOrgMountPath[tokenName] = vm.MountPath
			c.VolumeMounts[idx].MountPath = tokenMountPath
			podSpecVolumeMap[tokenVolumeName] = corev1.Volume{
				Name: tokenVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}
			preInitVolumeMountMap[tokenVolumeName] = corev1.VolumeMount{
				Name:      tokenVolumeName,
				MountPath: tokenMountPath,
			}
		case volume.Log != nil:
			logVolumeName := volume.Name
			logOrgMountPaths = append(logOrgMountPaths, vm.MountPath)
			c.VolumeMounts[idx].MountPath = logMountPath
			podSpecVolumeMap[logVolumeName] = corev1.Volume{
				Name: logVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}
			preInitVolumeMountMap[logVolumeName] = corev1.VolumeMount{
				Name:      logVolumeName,
				MountPath: logMountPath,
			}
		case volume.Report != nil:
			reportVolumeName := volume.Name
			reportOrgMountPaths = append(reportOrgMountPaths, vm.MountPath)
			c.VolumeMounts[idx].MountPath = reportMountPath
			podSpecVolumeMap[reportVolumeName] = corev1.Volume{
				Name: reportVolumeName,
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			}
			preInitVolumeMountMap[reportVolumeName] = corev1.VolumeMount{
				Name:      reportVolumeName,
				MountPath: reportMountPath,
			}
		default:
			podSpecVolumeMap[volume.Name] = corev1.Volume{
				Name:         volume.Name,
				VolumeSource: volume.VolumeSource,
			}
		}
	}
	return &TaskContainer{
		container:                  c,
		repoNameToArchiveMountPath: repoNameToArchiveMountPath,
		repoNameToOrgMountPath:     repoNameToOrgMountPath,
		tokenNameToMountPath:       tokenNameToMountPath,
		tokenNameToOrgMountPath:    tokenNameToOrgMountPath,
		artifactNameToMountPath:    artifactNameToMountPath,
		artifactNameToOrgMountPath: artifactNameToOrgMountPath,
		logOrgMountPaths:           logOrgMountPaths,
		reportOrgMountPaths:        reportOrgMountPaths,
		podSpecVolumeMap:           podSpecVolumeMap,
		preInitVolumeMountMap:      preInitVolumeMountMap,
	}
}
