package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func testjobObjectMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{
		GenerateName: "testjob-",
		Namespace:    "default",
	}
}

func testRepos() []RepositorySpec {
	return []RepositorySpec{
		{
			Name: "repo",
			Value: Repository{
				URL: "https://github.com/goccy/kubetest.git",
			},
		},
	}
}

func testRepoVolume() TestJobVolume {
	return TestJobVolume{
		Name: "repo-volume",
		TestJobVolumeSource: TestJobVolumeSource{
			Repo: &RepositoryVolumeSource{
				Name: "repo",
			},
		},
	}
}

func testRepoVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{
		Name:      "repo-volume",
		MountPath: filepath.Join("/", "work"),
	}
}

func TestRunner(t *testing.T) {
	t.Run("simple", func(t *testing.T) {
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				if _, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test",
							},
							Spec: TestJobPodSpec{
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:         "test",
											Image:        "alpine",
											Command:      []string{"echo"},
											Args:         []string{"hello"},
											WorkingDir:   filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{testRepoVolumeMount()},
										},
									},
								},
								Volumes: []TestJobVolume{testRepoVolume()},
							},
						},
					},
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("use token", func(t *testing.T) {
		if !inCluster {
			privateKeyPath := filepath.Join("..", "..", "testdata", "githubapp.private-key.pem")
			privateKey, err := ioutil.ReadFile(privateKeyPath)
			if err != nil {
				t.Fatal(err)
			}
			clientset, err := kubernetes.NewForConfig(getConfig())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := clientset.CoreV1().
				Secrets("default").
				Create(context.Background(), &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name: "github-app",
					},
					Data: map[string][]byte{
						"private-key": []byte(privateKey),
					},
				}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				if _, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						Tokens: []TokenSpec{
							{
								Name: "github-app-token",
								Value: TokenSource{
									GitHubApp: &GitHubAppTokenSource{
										AppID:        134426,
										Organization: "goccy",
										KeyFile: &corev1.SecretKeySelector{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "github-app",
											},
											Key: "private-key",
										},
									},
								},
							},
						},
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test-",
							},
							Spec: TestJobPodSpec{
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:       "test",
											Image:      "alpine",
											Command:    []string{"cat"},
											Args:       []string{filepath.Join("./github-token")},
											WorkingDir: filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{
												testRepoVolumeMount(),
												{
													Name:      "token-volume",
													MountPath: filepath.Join("/", "work", "github-token"),
												},
											},
										},
									},
								},
								Volumes: []TestJobVolume{
									testRepoVolume(),
									{
										Name: "token-volume",
										TestJobVolumeSource: TestJobVolumeSource{
											Token: &TokenVolumeSource{
												Name: "github-app-token",
											},
										},
									},
								},
							},
						},
					},
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("prestep", func(t *testing.T) {
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				if _, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						PreSteps: []PreStep{
							{
								Name: "build",
								Template: TestJobTemplateSpec{
									ObjectMeta: metav1.ObjectMeta{
										GenerateName: "build-",
									},
									Spec: TestJobPodSpec{
										Artifacts: []ArtifactSpec{
											{
												Name: "build-test",
												Container: ArtifactContainer{
													Name: "build",
													Path: filepath.Join("/", "work", "v1.test"),
												},
											},
										},
										PodSpec: corev1.PodSpec{
											Containers: []corev1.Container{
												{
													Name:    "build",
													Image:   "golang:1.17",
													Command: []string{"go"},
													Args: []string{
														"test",
														"-c",
														"./api/v1",
													},
													WorkingDir:   filepath.Join("/", "work"),
													VolumeMounts: []corev1.VolumeMount{testRepoVolumeMount()},
												},
											},
										},
										Volumes: []TestJobVolume{testRepoVolume()},
									},
								},
							},
						},
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test-",
							},
							Spec: TestJobPodSpec{
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:       "test",
											Image:      "alpine",
											Command:    []string{"ls"},
											Args:       []string{"-alh", "./compiled.test"},
											WorkingDir: filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{
												testRepoVolumeMount(),
												{
													Name:      "build-artifact",
													MountPath: filepath.Join("/", "work", "compiled.test"),
												},
											},
										},
									},
								},
								Volumes: []TestJobVolume{
									testRepoVolume(),
									{
										Name: "build-artifact",
										TestJobVolumeSource: TestJobVolumeSource{
											Artifact: &ArtifactVolumeSource{
												Name: "build-test",
											},
										},
									},
								},
							},
						},
					},
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("static key based multiple tasks", func(t *testing.T) {
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				if _, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						Strategy: &Strategy{
							Key: StrategyKeySpec{
								Env: "TEST",
								Source: StrategyKeySource{
									Static: []string{"A", "B", "C"},
								},
							},
							Scheduler: Scheduler{
								MaxContainersPerPod:    10,
								MaxConcurrentNumPerPod: 10,
							},
						},
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								GenerateName: "test-",
							},
							Spec: TestJobPodSpec{
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:       "test",
											Image:      "alpine",
											Command:    []string{"sh", "-c"},
											Args:       []string{"echo $TEST"},
											WorkingDir: filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{
												testRepoVolumeMount(),
											},
										},
									},
								},
								Volumes: []TestJobVolume{
									testRepoVolume(),
								},
							},
						},
					},
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("dynamic key based multiple tasks", func(t *testing.T) {
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				if _, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						Strategy: &Strategy{
							Key: StrategyKeySpec{
								Env: "TEST",
								Source: StrategyKeySource{
									Dynamic: &StrategyDynamicKeySource{
										Template: TestJobTemplateSpec{
											ObjectMeta: metav1.ObjectMeta{
												Name: "list",
											},
											Spec: TestJobPodSpec{
												PodSpec: corev1.PodSpec{
													Containers: []corev1.Container{
														{
															Name:    "list",
															Image:   "alpine",
															Command: []string{"sh", "-c"},
															Args: []string{
																fmt.Sprintf(
																	`echo "%s"`,
																	string([]byte{
																		'A', '\n',
																		'B', '\n',
																		'C', '\n',
																		'D',
																	}),
																),
															},
														},
													},
												},
											},
										},
									},
								},
							},
							Scheduler: Scheduler{
								MaxContainersPerPod:    10,
								MaxConcurrentNumPerPod: 10,
							},
						},
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test",
							},
							Spec: TestJobPodSpec{
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:       "test",
											Image:      "alpine",
											Command:    []string{"sh", "-c"},
											Args:       []string{"echo $TEST"},
											WorkingDir: filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{
												testRepoVolumeMount(),
											},
										},
									},
								},
								Volumes: []TestJobVolume{
									testRepoVolume(),
								},
							},
						},
					},
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("export artifacts by multiple tasks", func(t *testing.T) {
		for _, runMode := range getRunModes() {
			t.Run(runMode.String(), func(t *testing.T) {
				exportDir, err := os.MkdirTemp("", "exported_artifacts")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(exportDir)

				runner := NewRunner(getConfig(), runMode)
				runner.SetLogger(NewLogger(os.Stdout, LogLevelDebug))
				result, err := runner.Run(context.Background(), TestJob{
					ObjectMeta: testjobObjectMeta(),
					Spec: TestJobSpec{
						Repos: testRepos(),
						Strategy: &Strategy{
							Key: StrategyKeySpec{
								Env: "TEST",
								Source: StrategyKeySource{
									Static: []string{"A", "B", "C"},
								},
							},
							Scheduler: Scheduler{
								MaxContainersPerPod:    10,
								MaxConcurrentNumPerPod: 10,
							},
						},
						ExportArtifacts: []ExportArtifact{
							{
								Name: "export-artifact",
								Path: exportDir,
							},
						},
						Template: TestJobTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test",
							},
							Spec: TestJobPodSpec{
								Artifacts: []ArtifactSpec{
									{
										Name: "export-artifact",
										Container: ArtifactContainer{
											Name: "test",
											Path: filepath.Join("/", "work", "artifact"),
										},
									},
								},
								PodSpec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:       "test",
											Image:      "alpine",
											Command:    []string{"touch"},
											Args:       []string{"artifact"},
											WorkingDir: filepath.Join("/", "work"),
											VolumeMounts: []corev1.VolumeMount{
												testRepoVolumeMount(),
											},
										},
									},
								},
								Volumes: []TestJobVolume{
									testRepoVolume(),
								},
							},
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				artifacts, err := filepath.Glob(filepath.Join(exportDir, "*"))
				if err != nil {
					t.Fatal(err)
				}
				t.Log(artifacts)
				if runMode != RunModeDryRun && len(artifacts) != 3 {
					t.Fatalf("failed to find exported artifacts. artifacts num %d", len(artifacts))
				}
				b, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				t.Log(string(b))
			})
		}
	})
}