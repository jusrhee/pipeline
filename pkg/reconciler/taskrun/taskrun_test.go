/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package taskrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	tb "github.com/tektoncd/pipeline/internal/builder/v1beta1"
	"github.com/tektoncd/pipeline/pkg/apis/config"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	resourcev1alpha1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	podconvert "github.com/tektoncd/pipeline/pkg/pod"
	"github.com/tektoncd/pipeline/pkg/reconciler"
	"github.com/tektoncd/pipeline/pkg/reconciler/events/cloudevent"
	ttesting "github.com/tektoncd/pipeline/pkg/reconciler/testing"
	"github.com/tektoncd/pipeline/pkg/reconciler/volumeclaim"
	"github.com/tektoncd/pipeline/pkg/system"
	test "github.com/tektoncd/pipeline/test"
	"github.com/tektoncd/pipeline/test/diff"
	"github.com/tektoncd/pipeline/test/names"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sruntimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	fakekubeclientset "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	duckv1beta1 "knative.dev/pkg/apis/duck/v1beta1"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
)

const (
	entrypointLocation      = "/tekton/tools/entrypoint"
	taskNameLabelKey        = pipeline.GroupName + pipeline.TaskLabelKey
	clusterTaskNameLabelKey = pipeline.GroupName + pipeline.ClusterTaskLabelKey
	taskRunNameLabelKey     = pipeline.GroupName + pipeline.TaskRunLabelKey
	workspaceDir            = "/workspace"
	currentAPIVersion       = "tekton.dev/v1beta1"
)

var (
	namespace = "" // all namespaces
	images    = pipeline.Images{
		EntrypointImage:          "override-with-entrypoint:latest",
		NopImage:                 "tianon/true",
		AffinityAssistantImage:   "nginx",
		GitImage:                 "override-with-git:latest",
		CredsImage:               "override-with-creds:latest",
		KubeconfigWriterImage:    "override-with-kubeconfig-writer:latest",
		ShellImage:               "busybox",
		GsutilImage:              "google/cloud-sdk",
		BuildGCSFetcherImage:     "gcr.io/cloud-builders/gcs-fetcher:latest",
		PRImage:                  "override-with-pr:latest",
		ImageDigestExporterImage: "override-with-imagedigest-exporter-image:latest",
	}
	ignoreLastTransitionTime = cmpopts.IgnoreTypes(apis.Condition{}.LastTransitionTime.Inner.Time)
	// Pods are created with a random 5-character suffix that we want to
	// ignore in our diffs.
	ignoreRandomPodNameSuffix = cmp.FilterPath(func(path cmp.Path) bool {
		return path.GoString() == "{v1.ObjectMeta}.Name"
	}, cmp.Comparer(func(name1, name2 string) bool {
		return name1[:len(name1)-5] == name2[:len(name2)-5]
	}))
	resourceQuantityCmp = cmp.Comparer(func(x, y resource.Quantity) bool {
		return x.Cmp(y) == 0
	})
	cloudEventTarget1 = "https://foo"
	cloudEventTarget2 = "https://bar"

	simpleStep        = tb.Step("foo", tb.StepName("simple-step"), tb.StepCommand("/mycmd"))
	simpleTask        = tb.Task("test-task", tb.TaskSpec(simpleStep), tb.TaskNamespace("foo"))
	taskMultipleSteps = tb.Task("test-task-multi-steps", tb.TaskSpec(
		tb.Step("foo", tb.StepName("z-step"),
			tb.StepCommand("/mycmd"),
		),
		tb.Step("foo", tb.StepName("v-step"),
			tb.StepCommand("/mycmd"),
		),
		tb.Step("foo", tb.StepName("x-step"),
			tb.StepCommand("/mycmd"),
		),
	), tb.TaskNamespace("foo"))
	clustertask = tb.ClusterTask("test-cluster-task", tb.ClusterTaskSpec(simpleStep))
	taskSidecar = tb.Task("test-task-sidecar", tb.TaskSpec(
		tb.Sidecar("sidecar", "image-id"),
	), tb.TaskNamespace("foo"))
	taskMultipleSidecars = tb.Task("test-task-sidecar", tb.TaskSpec(
		tb.Sidecar("sidecar", "image-id"),
		tb.Sidecar("sidecar2", "image-id"),
	), tb.TaskNamespace("foo"))

	outputTask = tb.Task("test-output-task", tb.TaskSpec(
		simpleStep, tb.TaskResources(
			tb.TaskResourcesInput(gitResource.Name, resourcev1alpha1.PipelineResourceTypeGit),
			tb.TaskResourcesInput(anotherGitResource.Name, resourcev1alpha1.PipelineResourceTypeGit),
		),
		tb.TaskResources(tb.TaskResourcesOutput(gitResource.Name, resourcev1alpha1.PipelineResourceTypeGit)),
	))

	saTask = tb.Task("test-with-sa", tb.TaskSpec(tb.Step("foo", tb.StepName("sa-step"), tb.StepCommand("/mycmd"))), tb.TaskNamespace("foo"))

	templatedTask = tb.Task("test-task-with-substitution", tb.TaskSpec(
		tb.TaskParam("myarg", v1beta1.ParamTypeString),
		tb.TaskParam("myarghasdefault", v1beta1.ParamTypeString, tb.ParamSpecDefault("dont see me")),
		tb.TaskParam("myarghasdefault2", v1beta1.ParamTypeString, tb.ParamSpecDefault("thedefault")),
		tb.TaskParam("configmapname", v1beta1.ParamTypeString),
		tb.TaskResources(
			tb.TaskResourcesInput("workspace", resourcev1alpha1.PipelineResourceTypeGit),
			tb.TaskResourcesOutput("myimage", resourcev1alpha1.PipelineResourceTypeImage),
		),
		tb.Step("myimage", tb.StepName("mycontainer"), tb.StepCommand("/mycmd"), tb.StepArgs(
			"--my-arg=$(inputs.params.myarg)",
			"--my-arg-with-default=$(inputs.params.myarghasdefault)",
			"--my-arg-with-default2=$(inputs.params.myarghasdefault2)",
			"--my-additional-arg=$(outputs.resources.myimage.url)",
		)),
		tb.Step("myotherimage", tb.StepName("myothercontainer"), tb.StepCommand("/mycmd"), tb.StepArgs(
			"--my-other-arg=$(inputs.resources.workspace.url)",
		)),
		tb.TaskVolume("volume-configmap", tb.VolumeSource(corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: "$(inputs.params.configmapname)",
				},
			},
		})),
	), tb.TaskNamespace("foo"))

	twoOutputsTask = tb.Task("test-two-output-task", tb.TaskSpec(
		simpleStep, tb.TaskResources(
			tb.TaskResourcesOutput(cloudEventResource.Name, resourcev1alpha1.PipelineResourceTypeCloudEvent),
			tb.TaskResourcesOutput(anotherCloudEventResource.Name, resourcev1alpha1.PipelineResourceTypeCloudEvent),
		),
	), tb.TaskNamespace("foo"))

	gitResource = tb.PipelineResource("git-resource", tb.PipelineResourceNamespace("foo"), tb.PipelineResourceSpec(
		resourcev1alpha1.PipelineResourceTypeGit, tb.PipelineResourceSpecParam("URL", "https://foo.git"),
	))
	anotherGitResource = tb.PipelineResource("another-git-resource", tb.PipelineResourceNamespace("foo"), tb.PipelineResourceSpec(
		resourcev1alpha1.PipelineResourceTypeGit, tb.PipelineResourceSpecParam("URL", "https://foobar.git"),
	))
	imageResource = tb.PipelineResource("image-resource", tb.PipelineResourceNamespace("foo"), tb.PipelineResourceSpec(
		resourcev1alpha1.PipelineResourceTypeImage, tb.PipelineResourceSpecParam("URL", "gcr.io/kristoff/sven"),
	))
	cloudEventResource = tb.PipelineResource("cloud-event-resource", tb.PipelineResourceNamespace("foo"), tb.PipelineResourceSpec(
		resourcev1alpha1.PipelineResourceTypeCloudEvent, tb.PipelineResourceSpecParam("TargetURI", cloudEventTarget1),
	))
	anotherCloudEventResource = tb.PipelineResource("another-cloud-event-resource", tb.PipelineResourceNamespace("foo"), tb.PipelineResourceSpec(
		resourcev1alpha1.PipelineResourceTypeCloudEvent, tb.PipelineResourceSpecParam("TargetURI", cloudEventTarget2),
	))

	toolsVolume = corev1.Volume{
		Name: "tekton-internal-tools",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	workspaceVolume = corev1.Volume{
		Name: "tekton-internal-workspace",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	homeVolume = corev1.Volume{
		Name: "tekton-internal-home",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	resultsVolume = corev1.Volume{
		Name: "tekton-internal-results",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
	downwardVolume = corev1.Volume{
		Name: "tekton-internal-downward",
		VolumeSource: corev1.VolumeSource{
			DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{{
					Path: "ready",
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.annotations['tekton.dev/ready']",
					},
				}},
			},
		},
	}
	credsVolume = corev1.Volume{
		Name: "tekton-creds-init-home",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		},
	}

	getMkdirResourceContainer = func(name, dir, suffix string, ops ...tb.ContainerOp) tb.PodSpecOp {
		actualOps := []tb.ContainerOp{
			tb.Command("/tekton/tools/entrypoint"),
			tb.Args("-wait_file",
				"/tekton/downward/ready",
				"-wait_file_content",
				"-post_file",
				"/tekton/tools/0",
				"-termination_path",
				"/tekton/termination",
				"-entrypoint",
				"mkdir",
				"--",
				"-p",
				dir),
			tb.WorkingDir(workspaceDir),
			tb.EnvVar("HOME", "/tekton/home"),
			tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
			tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
			tb.VolumeMount("tekton-internal-workspace", workspaceDir),
			tb.VolumeMount("tekton-internal-home", "/tekton/home"),
			tb.VolumeMount("tekton-internal-results", "/tekton/results"),
			tb.TerminationMessagePath("/tekton/termination"),
		}

		actualOps = append(actualOps, ops...)

		return tb.PodContainer(fmt.Sprintf("step-create-dir-%s-%s", name, suffix), "busybox", actualOps...)
	}

	getPlaceToolsInitContainer = func(ops ...tb.ContainerOp) tb.PodSpecOp {
		actualOps := []tb.ContainerOp{
			tb.Command("cp", "/ko-app/entrypoint", entrypointLocation),
			tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
			tb.Args(),
		}
		actualOps = append(actualOps, ops...)
		return tb.PodInitContainer("place-tools", "override-with-entrypoint:latest", actualOps...)
	}
)

func getRunName(tr *v1beta1.TaskRun) string {
	return strings.Join([]string{tr.Namespace, tr.Name}, "/")
}

func ensureConfigurationConfigMapsExist(d *test.Data) {
	var defaultsExists, featureFlagsExists bool
	for _, cm := range d.ConfigMaps {
		if cm.Name == config.GetDefaultsConfigName() {
			defaultsExists = true
		}
		if cm.Name == config.GetFeatureFlagsConfigName() {
			featureFlagsExists = true
		}
	}
	if !defaultsExists {
		d.ConfigMaps = append(d.ConfigMaps, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.GetDefaultsConfigName(), Namespace: system.GetNamespace()},
			Data:       map[string]string{},
		})
	}
	if !featureFlagsExists {
		d.ConfigMaps = append(d.ConfigMaps, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: config.GetFeatureFlagsConfigName(), Namespace: system.GetNamespace()},
			Data:       map[string]string{},
		})
	}
}

// getTaskRunController returns an instance of the TaskRun controller/reconciler that has been seeded with
// d, where d represents the state of the system (existing resources) needed for the test.
func getTaskRunController(t *testing.T, d test.Data) (test.Assets, func()) {
	//unregisterMetrics()
	ctx, _ := ttesting.SetupFakeContext(t)
	ctx, cancel := context.WithCancel(ctx)
	ensureConfigurationConfigMapsExist(&d)
	c, informers := test.SeedTestData(t, ctx, d)
	configMapWatcher := configmap.NewInformedWatcher(c.Kube, system.GetNamespace())

	ctl := NewController(namespace, images)(ctx, configMapWatcher)
	if err := configMapWatcher.Start(ctx.Done()); err != nil {
		t.Fatalf("error starting configmap watcher: %v", err)
	}

	return test.Assets{
		Logger:     logging.FromContext(ctx),
		Controller: ctl,
		Clients:    c,
		Informers:  informers,
		Recorder:   controller.GetEventRecorder(ctx).(*record.FakeRecorder),
	}, cancel
}

func checkEvents(fr *record.FakeRecorder, testName string, wantEvents []string) error {
	// The fake recorder runs in a go routine, so the timeout is here to avoid waiting
	// on the channel forever if fewer than expected events are received.
	// We only hit the timeout in case of failure of the test, so the actual value
	// of the timeout is not so relevant, it's only used when tests are going to fail.
	// on the channel forever if fewer than expected events are received
	timer := time.NewTimer(1 * time.Second)
	foundEvents := []string{}
	for ii := 0; ii < len(wantEvents)+1; ii++ {
		// We loop over all the events that we expect. Once they are all received
		// we exit the loop. If we never receive enough events, the timeout takes us
		// out of the loop.
		select {
		case event := <-fr.Events:
			foundEvents = append(foundEvents, event)
			if ii > len(wantEvents)-1 {
				return fmt.Errorf("Received event \"%s\" for %s but not more expected", event, testName)
			}
			wantEvent := wantEvents[ii]
			if !(strings.HasPrefix(event, wantEvent)) {
				return fmt.Errorf("Expected event \"%s\" but got \"%s\" instead for %s", wantEvent, event, testName)
			}
		case <-timer.C:
			if len(foundEvents) > len(wantEvents) {
				return fmt.Errorf("Received %d events for %s but %d expected. Found events: %#v", len(foundEvents), testName, len(wantEvents), foundEvents)
			}
		}
	}
	return nil
}

func TestReconcile_ExplicitDefaultSA(t *testing.T) {
	taskRunSuccess := tb.TaskRun("test-taskrun-run-success", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name, tb.TaskRefAPIVersion("a1")),
	))
	taskRunWithSaSuccess := tb.TaskRun("test-taskrun-with-sa-run-success", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(saTask.Name, tb.TaskRefAPIVersion("a1")),
		tb.TaskRunServiceAccountName("test-sa"),
	))
	taskruns := []*v1beta1.TaskRun{taskRunSuccess, taskRunWithSaSuccess}
	defaultSAName := "pipelines"
	d := test.Data{
		TaskRuns: taskruns,
		Tasks:    []*v1beta1.Task{simpleTask, saTask},
		ConfigMaps: []*corev1.ConfigMap{
			{
				ObjectMeta: metav1.ObjectMeta{Name: config.GetDefaultsConfigName(), Namespace: system.GetNamespace()},
				Data: map[string]string{
					"default-service-account":        defaultSAName,
					"default-timeout-minutes":        "60",
					"default-managed-by-label-value": "tekton-pipelines",
				},
			},
		},
	}
	for _, tc := range []struct {
		name    string
		taskRun *v1beta1.TaskRun
		wantPod *corev1.Pod
	}{{
		name:    "success",
		taskRun: taskRunSuccess,
		wantPod: tb.Pod("test-taskrun-run-success-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-run-success"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-run-success",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodServiceAccountName(defaultSAName),
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "serviceaccount",
		taskRun: taskRunWithSaSuccess,
		wantPod: tb.Pod("test-taskrun-with-sa-run-success-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-with-sa"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-sa-run-success"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-sa-run-success",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodServiceAccountName("test-sa"),
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-sa-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			names.TestingSeed()
			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients
			saName := tc.taskRun.Spec.ServiceAccountName
			if saName == "" {
				saName = defaultSAName
			}
			t.Logf("Creating SA %s in %s", saName, tc.taskRun.Namespace)
			if _, err := clients.Kube.CoreV1().ServiceAccounts(tc.taskRun.Namespace).Create(&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      saName,
					Namespace: tc.taskRun.Namespace,
				},
			}); err != nil {
				t.Fatal(err)
			}

			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
				t.Errorf("expected no error. Got error %v", err)
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Errorf("Expected actions to be logged in the kubeclient, got none")
			}

			tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting updated taskrun: %v", err)
			}
			condition := tr.Status.GetCondition(apis.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionUnknown {
				t.Errorf("Expected invalid TaskRun to have in progress status, but had %v", condition)
			}
			if condition != nil && condition.Reason != v1beta1.TaskRunReasonRunning.String() {
				t.Errorf("Expected reason %q but was %s", v1beta1.TaskRunReasonRunning.String(), condition.Reason)
			}

			if tr.Status.PodName == "" {
				t.Fatalf("Reconcile didn't set pod name")
			}

			pod, err := clients.Kube.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to fetch build pod: %v", err)
			}

			if d := cmp.Diff(tc.wantPod.ObjectMeta, pod.ObjectMeta, ignoreRandomPodNameSuffix); d != "" {
				t.Errorf("Pod metadata doesn't match %s", diff.PrintWantGot(d))
			}

			if d := cmp.Diff(tc.wantPod.Spec, pod.Spec, resourceQuantityCmp); d != "" {
				t.Errorf("Pod spec doesn't match, %s", diff.PrintWantGot(d))
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Fatalf("Expected actions to be logged in the kubeclient, got none")
			}
		})
	}
}

// TestReconcile_FeatureFlags tests taskruns with and without feature flags set
// to ensure the 'feature-flags' config map can be used to disable the
// corresponding behavior.
func TestReconcile_FeatureFlags(t *testing.T) {
	taskWithEnvVar := tb.Task("test-task-with-env-var",
		tb.TaskSpec(tb.Step("foo",
			tb.StepName("simple-step"), tb.StepCommand("/mycmd"), tb.StepEnvVar("foo", "bar"),
		)),
		tb.TaskNamespace("foo"),
	)
	taskRunWithDisableHomeEnv := tb.TaskRun("test-taskrun-run-home-env",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(tb.TaskRunTaskRef(taskWithEnvVar.Name)),
	)
	taskRunWithDisableWorkingDirOverwrite := tb.TaskRun("test-taskrun-run-working-dir",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(tb.TaskRunTaskRef(simpleTask.Name)),
	)
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRunWithDisableHomeEnv, taskRunWithDisableWorkingDirOverwrite},
		Tasks:    []*v1beta1.Task{simpleTask, taskWithEnvVar},
	}
	for _, tc := range []struct {
		name        string
		taskRun     *v1beta1.TaskRun
		featureFlag string
		wantPod     *corev1.Pod
	}{{
		name:        "disable-home-env-overwrite",
		taskRun:     taskRunWithDisableHomeEnv,
		featureFlag: "disable-home-env-overwrite",
		wantPod: tb.Pod("test-taskrun-run-home-env-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task-with-env-var"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-run-home-env"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-run-home-env",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, credsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("foo", "bar"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.VolumeMount("tekton-creds-init-home", "/tekton/creds"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:        "disable-working-dir-overwrite",
		taskRun:     taskRunWithDisableWorkingDirOverwrite,
		featureFlag: "disable-working-directory-overwrite",
		wantPod: tb.Pod("test-taskrun-run-working-dir-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-run-working-dir"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-run-working-dir",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			names.TestingSeed()
			d.ConfigMaps = []*corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: config.GetFeatureFlagsConfigName(), Namespace: system.GetNamespace()},
					Data: map[string]string{
						tc.featureFlag: "true",
					},
				},
			}
			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients
			saName := tc.taskRun.Spec.ServiceAccountName
			if saName == "" {
				saName = "default"
			}
			if _, err := clients.Kube.CoreV1().ServiceAccounts(tc.taskRun.Namespace).Create(&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      saName,
					Namespace: tc.taskRun.Namespace,
				},
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
				t.Errorf("expected no error. Got error %v", err)
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Errorf("Expected actions to be logged in the kubeclient, got none")
			}

			tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting updated taskrun: %v", err)
			}
			condition := tr.Status.GetCondition(apis.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionUnknown {
				t.Errorf("Expected invalid TaskRun to have in progress status, but had %v", condition)
			}
			if condition != nil && condition.Reason != v1beta1.TaskRunReasonRunning.String() {
				t.Errorf("Expected reason %q but was %s", v1beta1.TaskRunReasonRunning.String(), condition.Reason)
			}

			if tr.Status.PodName == "" {
				t.Fatalf("Reconcile didn't set pod name")
			}

			pod, err := clients.Kube.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to fetch build pod: %v", err)
			}

			if d := cmp.Diff(tc.wantPod.ObjectMeta, pod.ObjectMeta, ignoreRandomPodNameSuffix); d != "" {
				t.Errorf("Pod metadata doesn't match %s", diff.PrintWantGot(d))
			}

			if d := cmp.Diff(tc.wantPod.Spec, pod.Spec, resourceQuantityCmp); d != "" {
				t.Errorf("Pod spec doesn't match, %s", diff.PrintWantGot(d))
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Fatalf("Expected actions to be logged in the kubeclient, got none")
			}
		})
	}
}
func TestReconcile(t *testing.T) {
	taskRunSuccess := tb.TaskRun("test-taskrun-run-success", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name, tb.TaskRefAPIVersion("a1")),
	))
	taskRunWithSaSuccess := tb.TaskRun("test-taskrun-with-sa-run-success", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(saTask.Name, tb.TaskRefAPIVersion("a1")), tb.TaskRunServiceAccountName("test-sa"),
	))
	taskRunSubstitution := tb.TaskRun("test-taskrun-substitution", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(templatedTask.Name, tb.TaskRefAPIVersion("a1")),
		tb.TaskRunParam("myarg", "foo"),
		tb.TaskRunParam("myarghasdefault", "bar"),
		tb.TaskRunParam("configmapname", "configbar"),
		tb.TaskRunResources(
			tb.TaskRunResourcesInput("workspace", tb.TaskResourceBindingRef(gitResource.Name)),
			tb.TaskRunResourcesOutput("myimage", tb.TaskResourceBindingRef("image-resource")),
		),
	))
	taskRunInputOutput := tb.TaskRun("test-taskrun-input-output",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunOwnerReference("PipelineRun", "test"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(outputTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesInput(gitResource.Name,
					tb.TaskResourceBindingRef(gitResource.Name),
					tb.TaskResourceBindingPaths("source-folder"),
				),
				tb.TaskRunResourcesInput(anotherGitResource.Name,
					tb.TaskResourceBindingRef(anotherGitResource.Name),
					tb.TaskResourceBindingPaths("source-folder"),
				),
				tb.TaskRunResourcesOutput(gitResource.Name,
					tb.TaskResourceBindingRef(gitResource.Name),
					tb.TaskResourceBindingPaths("output-folder"),
				),
			),
		),
	)
	taskRunWithTaskSpec := tb.TaskRun("test-taskrun-with-taskspec", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunParam("myarg", "foo"),
		tb.TaskRunResources(
			tb.TaskRunResourcesInput("workspace", tb.TaskResourceBindingRef(gitResource.Name)),
		),
		tb.TaskRunTaskSpec(
			tb.TaskParam("myarg", v1beta1.ParamTypeString, tb.ParamSpecDefault("mydefault")),
			tb.TaskResources(
				tb.TaskResourcesInput("workspace", resourcev1alpha1.PipelineResourceTypeGit),
			),
			tb.Step("myimage", tb.StepName("mycontainer"), tb.StepCommand("/mycmd"),
				tb.StepArgs("--my-arg=$(inputs.params.myarg)"),
			),
		),
	))

	taskRunWithResourceSpecAndTaskSpec := tb.TaskRun("test-taskrun-with-resource-spec", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunResources(
			tb.TaskRunResourcesInput("workspace", tb.TaskResourceBindingResourceSpec(&resourcev1alpha1.PipelineResourceSpec{
				Type: resourcev1alpha1.PipelineResourceTypeGit,
				Params: []resourcev1alpha1.ResourceParam{{
					Name:  "URL",
					Value: "github.com/foo/bar.git",
				}, {
					Name:  "revision",
					Value: "rel-can",
				}},
			})),
		),
		tb.TaskRunTaskSpec(
			tb.TaskResources(
				tb.TaskResourcesInput("workspace", resourcev1alpha1.PipelineResourceTypeGit)),
			tb.Step("ubuntu", tb.StepName("mystep"), tb.StepCommand("/mycmd")),
		),
	))

	taskRunWithClusterTask := tb.TaskRun("test-taskrun-with-cluster-task",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(tb.TaskRunTaskRef(clustertask.Name, tb.TaskRefKind(v1beta1.ClusterTaskKind))),
	)

	taskRunWithLabels := tb.TaskRun("test-taskrun-with-labels",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunLabel("TaskRunLabel", "TaskRunValue"),
		tb.TaskRunLabel(taskRunNameLabelKey, "WillNotBeUsed"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
		),
	)

	taskRunWithAnnotations := tb.TaskRun("test-taskrun-with-annotations",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunAnnotation("TaskRunAnnotation", "TaskRunValue"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
		),
	)

	taskRunWithPod := tb.TaskRun("test-taskrun-with-pod",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(tb.TaskRunTaskRef(simpleTask.Name)),
		tb.TaskRunStatus(tb.PodName("some-pod-abcdethat-no-longer-exists")),
	)

	taskRunWithCredentialsVariable := tb.TaskRun("test-taskrun-with-credentials-variable", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskSpec(
			tb.Step("myimage", tb.StepName("mycontainer"), tb.StepCommand("/mycmd $(credentials.path)")),
		),
	))

	taskruns := []*v1beta1.TaskRun{
		taskRunSuccess, taskRunWithSaSuccess,
		taskRunSubstitution, taskRunInputOutput,
		taskRunWithTaskSpec, taskRunWithClusterTask, taskRunWithResourceSpecAndTaskSpec,
		taskRunWithLabels, taskRunWithAnnotations, taskRunWithPod,
		taskRunWithCredentialsVariable,
	}

	d := test.Data{
		TaskRuns:          taskruns,
		Tasks:             []*v1beta1.Task{simpleTask, saTask, templatedTask, outputTask},
		ClusterTasks:      []*v1beta1.ClusterTask{clustertask},
		PipelineResources: []*resourcev1alpha1.PipelineResource{gitResource, anotherGitResource, imageResource},
	}
	for _, tc := range []struct {
		name       string
		taskRun    *v1beta1.TaskRun
		wantPod    *corev1.Pod
		wantEvents []string
	}{{
		name:    "success",
		taskRun: taskRunSuccess,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-run-success-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-run-success"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-run-success",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "serviceaccount",
		taskRun: taskRunWithSaSuccess,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-sa-run-success-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-with-sa"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-sa-run-success"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-sa-run-success",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodServiceAccountName("test-sa"),
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-sa-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "params",
		taskRun: taskRunSubstitution,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-substitution-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task-with-substitution"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-substitution"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-substitution",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(
					workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume,
					corev1.Volume{
						Name: "volume-configmap",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "configbar",
								},
							},
						},
					},
				),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				getMkdirResourceContainer("myimage", "/workspace/output/myimage", "mssqb"),
				tb.PodContainer("step-git-source-workspace-mz4c7", "override-with-git:latest",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file", "/tekton/tools/0", "-post_file", "/tekton/tools/1", "-termination_path",
						"/tekton/termination", "-entrypoint", "/ko-app/git-init", "--", "-url", "https://foo.git",
						"-path", "/workspace/workspace"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.EnvVar("TEKTON_RESOURCE_NAME", "workspace"),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
				tb.PodContainer("step-mycontainer", "myimage",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file", "/tekton/tools/1", "-post_file", "/tekton/tools/2", "-termination_path",
						"/tekton/termination", "-entrypoint", "/mycmd", "--", "--my-arg=foo", "--my-arg-with-default=bar",
						"--my-arg-with-default2=thedefault", "--my-additional-arg=gcr.io/kristoff/sven"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
				tb.PodContainer("step-myothercontainer", "myotherimage",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file", "/tekton/tools/2", "-post_file", "/tekton/tools/3", "-termination_path",
						"/tekton/termination", "-entrypoint", "/mycmd", "--", "--my-other-arg=https://foo.git"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
				tb.PodContainer("step-image-digest-exporter-9l9zj", "override-with-imagedigest-exporter-image:latest",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file", "/tekton/tools/3", "-post_file", "/tekton/tools/4", "-termination_path",
						"/tekton/termination", "-entrypoint", "/ko-app/imagedigestexporter", "--",
						"-images", "[{\"name\":\"myimage\",\"type\":\"image\",\"url\":\"gcr.io/kristoff/sven\",\"digest\":\"\",\"OutputImageDir\":\"/workspace/output/myimage\"}]"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "taskrun-with-taskspec",
		taskRun: taskRunWithTaskSpec,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-taskspec-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-taskspec"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-taskspec",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-git-source-workspace-9l9zj", "override-with-git:latest",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/ko-app/git-init",
						"--",
						"-url",
						"https://foo.git",
						"-path",
						"/workspace/workspace",
					),
					tb.WorkingDir(workspaceDir),
					// Note: the duplication of HOME env var here is intentional: our pod builder
					// adds it first and the git pipelineresource adds its own to ensure that HOME
					// is set even when disable-home-env-overwrite feature flag is "true".
					tb.EnvVar("HOME", "/tekton/home"),
					tb.EnvVar("TEKTON_RESOURCE_NAME", "workspace"),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
				tb.PodContainer("step-mycontainer", "myimage",
					tb.Command(entrypointLocation),
					tb.WorkingDir(workspaceDir),
					tb.Args("-wait_file", "/tekton/tools/0", "-post_file", "/tekton/tools/1", "-termination_path",
						"/tekton/termination", "-entrypoint", "/mycmd", "--", "--my-arg=foo"),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "success-with-cluster-task",
		taskRun: taskRunWithClusterTask,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-cluster-task-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-cluster-task"),
			tb.PodLabel(clusterTaskNameLabelKey, "test-cluster-task"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-cluster-task"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-cluster-task",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--",
					),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "taskrun-with-resource-spec-task-spec",
		taskRun: taskRunWithResourceSpecAndTaskSpec,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-resource-spec-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-resource-spec"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-resource-spec",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-git-source-workspace-9l9zj", "override-with-git:latest",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/ko-app/git-init",
						"--",
						"-url",
						"github.com/foo/bar.git",
						"-path",
						"/workspace/workspace",
						"-revision",
						"rel-can"),
					tb.WorkingDir(workspaceDir),
					// Note: the duplication of HOME env var here is intentional: our pod builder
					// adds it first and the git pipelineresource adds its own to ensure that HOME
					// is set even when disable-home-env-overwrite feature flag is "true".
					tb.EnvVar("HOME", "/tekton/home"),
					tb.EnvVar("TEKTON_RESOURCE_NAME", "workspace"),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
				tb.PodContainer("step-mystep", "ubuntu",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file", "/tekton/tools/0", "-post_file", "/tekton/tools/1", "-termination_path",
						"/tekton/termination", "-entrypoint", "/mycmd", "--"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "taskrun-with-pod",
		taskRun: taskRunWithPod,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-pod-pod-abcde",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskNameLabelKey, "test-task"),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-pod"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-pod",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-simple-step", "foo",
					tb.Command(entrypointLocation),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						"/mycmd",
						"--"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}, {
		name:    "taskrun-with-credentials-variable-default-tekton-home",
		taskRun: taskRunWithCredentialsVariable,
		wantEvents: []string{
			"Normal Started ",
			"Normal Running Not all Steps",
		},
		wantPod: tb.Pod("test-taskrun-with-credentials-variable-pod-9l9zj",
			tb.PodNamespace("foo"),
			tb.PodAnnotation(podconvert.ReleaseAnnotation, podconvert.ReleaseAnnotationValue),
			tb.PodLabel(taskRunNameLabelKey, "test-taskrun-with-credentials-variable"),
			tb.PodLabel("app.kubernetes.io/managed-by", "tekton-pipelines"),
			tb.PodOwnerReference("TaskRun", "test-taskrun-with-credentials-variable",
				tb.OwnerReferenceAPIVersion(currentAPIVersion)),
			tb.PodSpec(
				tb.PodVolumes(workspaceVolume, homeVolume, resultsVolume, toolsVolume, downwardVolume),
				tb.PodRestartPolicy(corev1.RestartPolicyNever),
				getPlaceToolsInitContainer(),
				tb.PodContainer("step-mycontainer", "myimage",
					tb.Command("/tekton/tools/entrypoint"),
					tb.Args("-wait_file",
						"/tekton/downward/ready",
						"-wait_file_content",
						"-post_file",
						"/tekton/tools/0",
						"-termination_path",
						"/tekton/termination",
						"-entrypoint",
						// Important bit here: /tekton/home
						"/mycmd /tekton/home",
						"--"),
					tb.WorkingDir(workspaceDir),
					tb.EnvVar("HOME", "/tekton/home"),
					tb.VolumeMount("tekton-internal-tools", "/tekton/tools"),
					tb.VolumeMount("tekton-internal-downward", "/tekton/downward"),
					tb.VolumeMount("tekton-internal-workspace", workspaceDir),
					tb.VolumeMount("tekton-internal-home", "/tekton/home"),
					tb.VolumeMount("tekton-internal-results", "/tekton/results"),
					tb.TerminationMessagePath("/tekton/termination"),
				),
			),
		),
	}} {
		t.Run(tc.name, func(t *testing.T) {
			names.TestingSeed()
			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients
			saName := tc.taskRun.Spec.ServiceAccountName
			if saName == "" {
				saName = "default"
			}
			if _, err := clients.Kube.CoreV1().ServiceAccounts(tc.taskRun.Namespace).Create(&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      saName,
					Namespace: tc.taskRun.Namespace,
				},
			}); err != nil {
				t.Fatal(err)
			}

			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
				t.Errorf("expected no error. Got error %v", err)
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Errorf("Expected actions to be logged in the kubeclient, got none")
			}

			tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting updated taskrun: %v", err)
			}
			condition := tr.Status.GetCondition(apis.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionUnknown {
				t.Errorf("Expected invalid TaskRun to have in progress status, but had %v", condition)
			}
			if condition != nil && condition.Reason != v1beta1.TaskRunReasonRunning.String() {
				t.Errorf("Expected reason %q but was %s", v1beta1.TaskRunReasonRunning.String(), condition.Reason)
			}

			if tr.Status.PodName == "" {
				t.Fatalf("Reconcile didn't set pod name")
			}

			pod, err := clients.Kube.CoreV1().Pods(tr.Namespace).Get(tr.Status.PodName, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to fetch build pod: %v", err)
			}

			if d := cmp.Diff(tc.wantPod.ObjectMeta, pod.ObjectMeta, ignoreRandomPodNameSuffix); d != "" {
				t.Errorf("Pod metadata doesn't match %s", diff.PrintWantGot(d))
			}

			pod.Name = tc.wantPod.Name // Ignore pod name differences, the pod name is generated and tested in pod_test.go
			if d := cmp.Diff(tc.wantPod.Spec, pod.Spec, resourceQuantityCmp); d != "" {
				t.Errorf("Pod spec doesn't match %s", diff.PrintWantGot(d))
			}
			if len(clients.Kube.Actions()) == 0 {
				t.Fatalf("Expected actions to be logged in the kubeclient, got none")
			}

			err = checkEvents(testAssets.Recorder, tc.name, tc.wantEvents)
			if !(err == nil) {
				t.Errorf(err.Error())
			}
		})
	}
}

func TestReconcile_SetsStartTime(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name),
	))
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
	}
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	newTr, err := testAssets.Clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	if newTr.Status.StartTime == nil || newTr.Status.StartTime.IsZero() {
		t.Errorf("expected startTime to be set by reconcile but was %q", newTr.Status.StartTime)
	}
}

func TestReconcile_SortTaskRunStatusSteps(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(taskMultipleSteps.Name)),
		tb.TaskRunStatus(
			tb.PodName("the-pod"),
		),
	)

	// The order of the container statuses has been shuffled, not aligning with the order of the
	// spec steps of the Task any more. After Reconcile is called, we should see the order of status
	// steps in TaksRun has been converted to the same one as in spec steps of the Task.
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{taskMultipleSteps},
		Pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "the-pod",
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodSucceeded,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "step-nop",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
						},
					},
				}, {
					Name: "step-x-step",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
						},
					},
				}, {
					Name: "step-v-step",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
						},
					},
				}, {
					Name: "step-z-step",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
						},
					},
				}},
			},
		}},
	}
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	newTr, err := testAssets.Clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	verifyTaskRunStatusStep(t, newTr)
}

func verifyTaskRunStatusStep(t *testing.T, taskRun *v1beta1.TaskRun) {
	actualStepOrder := []string{}
	for _, state := range taskRun.Status.Steps {
		actualStepOrder = append(actualStepOrder, state.Name)
	}
	expectedStepOrder := []string{}
	for _, state := range taskMultipleSteps.Spec.Steps {
		expectedStepOrder = append(expectedStepOrder, state.Name)
	}
	// Add a nop in the end. This may be removed in future.
	expectedStepOrder = append(expectedStepOrder, "nop")
	if d := cmp.Diff(expectedStepOrder, actualStepOrder); d != "" {
		t.Errorf("The status steps in TaksRun doesn't match the spec steps in Task %s", diff.PrintWantGot(d))
	}
}

func TestReconcile_DoesntChangeStartTime(t *testing.T) {
	startTime := time.Date(2000, 1, 1, 1, 1, 1, 1, time.UTC)
	taskRun := tb.TaskRun("test-taskrun", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name)),
		tb.TaskRunStatus(
			tb.TaskRunStartTime(startTime),
			tb.PodName("the-pod"),
		),
	)
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
		Pods: []*corev1.Pod{{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "the-pod",
			},
		}},
	}
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	if taskRun.Status.StartTime.Time != startTime {
		t.Errorf("expected startTime %q to be preserved by reconcile but was %q", startTime, taskRun.Status.StartTime)
	}
}

func TestReconcileInvalidTaskRuns(t *testing.T) {
	noTaskRun := tb.TaskRun("notaskrun", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(tb.TaskRunTaskRef("notask")))
	withWrongRef := tb.TaskRun("taskrun-with-wrong-ref", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef("taskrun-with-wrong-ref", tb.TaskRefKind(v1beta1.ClusterTaskKind)),
	))
	taskRuns := []*v1beta1.TaskRun{noTaskRun, withWrongRef}
	tasks := []*v1beta1.Task{simpleTask}

	d := test.Data{
		TaskRuns: taskRuns,
		Tasks:    tasks,
	}

	testcases := []struct {
		name       string
		taskRun    *v1beta1.TaskRun
		reason     string
		wantEvents []string
	}{{
		name:    "task run with no task",
		taskRun: noTaskRun,
		reason:  podconvert.ReasonFailedResolution,
		wantEvents: []string{
			"Normal Started ",
			"Warning Failed ",
		},
	}, {
		name:    "task run with wrong ref",
		taskRun: withWrongRef,
		reason:  podconvert.ReasonFailedResolution,
		wantEvents: []string{
			"Normal Started ",
			"Warning Failed ",
		},
	}}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients
			err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun))

			// When a TaskRun is invalid and can't run, we don't want to return an error because
			// an error will tell the Reconciler to keep trying to reconcile; instead we want to stop
			// and forget about the Run.
			if err != nil {
				t.Errorf("Did not expect to see error when reconciling invalid TaskRun but saw %q", err)
			}

			// Check actions and events
			actions := clients.Kube.Actions()
			if len(actions) != 3 || actions[0].Matches("namespaces", "list") {
				t.Errorf("expected 3 actions (first: list namespaces) created by the reconciler, got %d. Actions: %#v", len(actions), actions)
			}

			err = checkEvents(testAssets.Recorder, tc.name, tc.wantEvents)
			if !(err == nil) {
				t.Errorf(err.Error())
			}

			newTr, err := testAssets.Clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", tc.taskRun.Name, err)
			}
			// Since the TaskRun is invalid, the status should say it has failed
			condition := newTr.Status.GetCondition(apis.ConditionSucceeded)
			if condition == nil || condition.Status != corev1.ConditionFalse {
				t.Errorf("Expected invalid TaskRun to have failed status, but had %v", condition)
			}
			if condition != nil && condition.Reason != tc.reason {
				t.Errorf("Expected failure to be because of reason %q but was %s", tc.reason, condition.Reason)
			}
		})
	}

}

func TestReconcilePodFetchError(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-success",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(tb.TaskRunTaskRef("test-task")),
		tb.TaskRunStatus(tb.PodName("will-not-be-found")),
	)
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients

	clients.Kube.PrependReactor("get", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("induce failure fetching pods")
	})

	if err := c.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err == nil {
		t.Fatal("expected error when reconciling a Task for which we couldn't get the corresponding Pod but got nil")
	}
}

func makePod(taskRun *v1beta1.TaskRun, task *v1beta1.Task) (*corev1.Pod, error) {
	// TODO(jasonhall): This avoids a circular dependency where
	// getTaskRunController takes a test.Data which must be populated with
	// a pod created from MakePod which requires a (fake) Kube client. When
	// we remove Build entirely from this controller, we should simply
	// specify the Pod we want to exist directly, and not call MakePod from
	// the build. This will break the cycle and allow us to simply use
	// clients normally.
	kubeclient := fakekubeclientset.NewSimpleClientset(&corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default",
			Namespace: taskRun.Namespace,
		},
	})

	entrypointCache, err := podconvert.NewEntrypointCache(kubeclient)
	if err != nil {
		return nil, err
	}

	return podconvert.MakePod(context.Background(), images, taskRun, task.Spec, kubeclient, entrypointCache, true)
}

func TestReconcilePodUpdateStatus(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-success", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(tb.TaskRunTaskRef("test-task")))

	pod, err := makePod(taskRun, simpleTask)
	if err != nil {
		t.Fatalf("MakePod: %v", err)
	}
	taskRun.Status = v1beta1.TaskRunStatus{
		TaskRunStatusFields: v1beta1.TaskRunStatusFields{
			PodName: pod.Name,
		},
	}
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
		Pods:     []*corev1.Pod{pod},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Fatalf("Unexpected error when Reconcile() : %v", err)
	}
	newTr, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	if d := cmp.Diff(&apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  corev1.ConditionUnknown,
		Reason:  v1beta1.TaskRunReasonRunning.String(),
		Message: "Not all Steps in the Task have finished executing",
	}, newTr.Status.GetCondition(apis.ConditionSucceeded), ignoreLastTransitionTime); d != "" {
		t.Fatalf("Did not get expected condition %s", diff.PrintWantGot(d))
	}

	// update pod status and trigger reconcile : build is completed
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodSucceeded,
	}
	if _, err := clients.Kube.CoreV1().Pods(taskRun.Namespace).UpdateStatus(pod); err != nil {
		t.Errorf("Unexpected error while updating build: %v", err)
	}

	// Before calling Reconcile again, we need to ensure that the informer's
	// lister cache is update to reflect the result of the previous Reconcile.
	testAssets.Informers.TaskRun.Informer().GetIndexer().Add(newTr)

	if err := c.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Fatalf("Unexpected error when Reconcile(): %v", err)
	}

	newTr, err = clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error fetching taskrun: %v", err)
	}
	if d := cmp.Diff(&apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  corev1.ConditionTrue,
		Reason:  v1beta1.TaskRunReasonSuccessful.String(),
		Message: "All Steps have completed executing",
	}, newTr.Status.GetCondition(apis.ConditionSucceeded), ignoreLastTransitionTime); d != "" {
		t.Errorf("Did not get expected condition %s", diff.PrintWantGot(d))
	}

	wantEvents := []string{
		"Normal Started ",
		"Normal Running Not all Steps",
		"Normal Succeeded",
	}
	err = checkEvents(testAssets.Recorder, "test-reconcile-pod-updateStatus", wantEvents)
	if !(err == nil) {
		t.Errorf(err.Error())
	}
}

func TestReconcileOnCompletedTaskRun(t *testing.T) {
	taskSt := &apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  corev1.ConditionTrue,
		Reason:  "Build succeeded",
		Message: "Build succeeded",
	}
	taskRun := tb.TaskRun("test-taskrun-run-success", tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name),
	), tb.TaskRunStatus(tb.StatusCondition(*taskSt)))
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{
			taskRun,
		},
		Tasks: []*v1beta1.Task{simpleTask},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
	}
	newTr, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}
	if d := cmp.Diff(taskSt, newTr.Status.GetCondition(apis.ConditionSucceeded), ignoreLastTransitionTime); d != "" {
		t.Fatalf("Did not get expected condition %s", diff.PrintWantGot(d))
	}
}

func TestReconcileOnCancelledTaskRun(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-run-cancelled",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
			tb.TaskRunCancelled,
		), tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: corev1.ConditionUnknown,
		})))
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	c := testAssets.Controller
	clients := testAssets.Clients

	if err := c.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
	}
	newTr, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	expectedStatus := &apis.Condition{
		Type:    apis.ConditionSucceeded,
		Status:  corev1.ConditionFalse,
		Reason:  "TaskRunCancelled",
		Message: `TaskRun "test-taskrun-run-cancelled" was cancelled`,
	}
	if d := cmp.Diff(expectedStatus, newTr.Status.GetCondition(apis.ConditionSucceeded), ignoreLastTransitionTime); d != "" {
		t.Fatalf("Did not get expected condition %s", diff.PrintWantGot(d))
	}

	wantEvents := []string{
		"Normal Started",
		"Warning Failed TaskRun \"test-taskrun-run-cancelled\" was cancelled",
	}
	err = checkEvents(testAssets.Recorder, "test-reconcile-on-cancelled-taskrun", wantEvents)
	if !(err == nil) {
		t.Errorf(err.Error())
	}
}

func TestReconcileTimeouts(t *testing.T) {
	type testCase struct {
		taskRun        *v1beta1.TaskRun
		expectedStatus *apis.Condition
		wantEvents     []string
	}

	testcases := []testCase{
		{
			taskRun: tb.TaskRun("test-taskrun-timeout",
				tb.TaskRunNamespace("foo"),
				tb.TaskRunSpec(
					tb.TaskRunTaskRef(simpleTask.Name),
					tb.TaskRunTimeout(10*time.Second),
				),
				tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
					Type:   apis.ConditionSucceeded,
					Status: corev1.ConditionUnknown}),
					tb.TaskRunStartTime(time.Now().Add(-15*time.Second)))),

			expectedStatus: &apis.Condition{
				Type:    apis.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "TaskRunTimeout",
				Message: `TaskRun "test-taskrun-timeout" failed to finish within "10s"`,
			},
			wantEvents: []string{
				"Warning Failed ",
			},
		}, {
			taskRun: tb.TaskRun("test-taskrun-default-timeout-60-minutes",
				tb.TaskRunNamespace("foo"),
				tb.TaskRunSpec(
					tb.TaskRunTaskRef(simpleTask.Name),
				),
				tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
					Type:   apis.ConditionSucceeded,
					Status: corev1.ConditionUnknown}),
					tb.TaskRunStartTime(time.Now().Add(-61*time.Minute)))),

			expectedStatus: &apis.Condition{
				Type:    apis.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "TaskRunTimeout",
				Message: `TaskRun "test-taskrun-default-timeout-60-minutes" failed to finish within "1h0m0s"`,
			},
			wantEvents: []string{
				"Warning Failed ",
			},
		}, {
			taskRun: tb.TaskRun("test-taskrun-nil-timeout-default-60-minutes",
				tb.TaskRunNamespace("foo"),
				tb.TaskRunSpec(
					tb.TaskRunTaskRef(simpleTask.Name),
					tb.TaskRunNilTimeout,
				),
				tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
					Type:   apis.ConditionSucceeded,
					Status: corev1.ConditionUnknown}),
					tb.TaskRunStartTime(time.Now().Add(-61*time.Minute)))),

			expectedStatus: &apis.Condition{
				Type:    apis.ConditionSucceeded,
				Status:  corev1.ConditionFalse,
				Reason:  "TaskRunTimeout",
				Message: `TaskRun "test-taskrun-nil-timeout-default-60-minutes" failed to finish within "1h0m0s"`,
			},
			wantEvents: []string{
				"Warning Failed ",
			},
		}}

	for _, tc := range testcases {
		d := test.Data{
			TaskRuns: []*v1beta1.TaskRun{tc.taskRun},
			Tasks:    []*v1beta1.Task{simpleTask},
		}
		testAssets, cancel := getTaskRunController(t, d)
		defer cancel()
		c := testAssets.Controller
		clients := testAssets.Clients

		if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
			t.Fatalf("Unexpected error when reconciling completed TaskRun : %v", err)
		}
		newTr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", tc.taskRun.Name, err)
		}
		condition := newTr.Status.GetCondition(apis.ConditionSucceeded)
		if d := cmp.Diff(tc.expectedStatus, condition, ignoreLastTransitionTime); d != "" {
			t.Fatalf("Did not get expected condition %s", diff.PrintWantGot(d))
		}
		err = checkEvents(testAssets.Recorder, tc.taskRun.Name, tc.wantEvents)
		if !(err == nil) {
			t.Errorf(err.Error())
		}
	}
}

func TestHandlePodCreationError(t *testing.T) {
	taskRun := tb.TaskRun("test-taskrun-pod-creation-failed", tb.TaskRunSpec(
		tb.TaskRunTaskRef(simpleTask.Name),
	), tb.TaskRunStatus(
		tb.TaskRunStartTime(time.Now()),
		tb.StatusCondition(apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: corev1.ConditionUnknown,
		}),
	))
	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{simpleTask},
	}
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	// Use the test assets to create a *Reconciler directly for focused testing.
	c := &Reconciler{
		KubeClientSet:     testAssets.Clients.Kube,
		PipelineClientSet: testAssets.Clients.Pipeline,
		taskRunLister:     testAssets.Informers.TaskRun.Lister(),
		taskLister:        testAssets.Informers.Task.Lister(),
		clusterTaskLister: testAssets.Informers.ClusterTask.Lister(),
		resourceLister:    testAssets.Informers.PipelineResource.Lister(),
		timeoutHandler:    reconciler.NewTimeoutHandler(ctx.Done(), testAssets.Logger),
		cloudEventClient:  testAssets.Clients.CloudEvents,
		metrics:           nil, // Not used
		entrypointCache:   nil, // Not used
		pvcHandler:        volumeclaim.NewPVCHandler(testAssets.Clients.Kube, testAssets.Logger),
	}

	// Prevent backoff timer from starting
	c.timeoutHandler.SetTaskRunCallbackFunc(nil)

	testcases := []struct {
		description    string
		err            error
		expectedType   apis.ConditionType
		expectedStatus corev1.ConditionStatus
		expectedReason string
	}{{
		description:    "exceeded quota errors are surfaced in taskrun condition but do not fail taskrun",
		err:            k8sapierrors.NewForbidden(k8sruntimeschema.GroupResource{Group: "foo", Resource: "bar"}, "baz", errors.New("exceeded quota")),
		expectedType:   apis.ConditionSucceeded,
		expectedStatus: corev1.ConditionUnknown,
		expectedReason: podconvert.ReasonExceededResourceQuota,
	}, {
		description:    "errors other than exceeded quota fail the taskrun",
		err:            errors.New("this is a fatal error"),
		expectedType:   apis.ConditionSucceeded,
		expectedStatus: corev1.ConditionFalse,
		expectedReason: podconvert.ReasonCouldntGetTask,
	}}
	for _, tc := range testcases {
		t.Run(tc.description, func(t *testing.T) {
			c.handlePodCreationError(ctx, taskRun, tc.err)
			foundCondition := false
			for _, cond := range taskRun.Status.Conditions {
				if cond.Type == tc.expectedType && cond.Status == tc.expectedStatus && cond.Reason == tc.expectedReason {
					foundCondition = true
					break
				}
			}
			if !foundCondition {
				t.Errorf("expected to find condition type %q, status %q and reason %q", tc.expectedType, tc.expectedStatus, tc.expectedReason)
			}
		})
	}
}

func TestReconcileCloudEvents(t *testing.T) {

	taskRunWithNoCEResources := tb.TaskRun("test-taskrun-no-ce-resources",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name, tb.TaskRefAPIVersion("a1")),
		))
	taskRunWithTwoCEResourcesNoInit := tb.TaskRun("test-taskrun-two-ce-resources-no-init",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(twoOutputsTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesOutput(cloudEventResource.Name, tb.TaskResourceBindingRef(cloudEventResource.Name)),
				tb.TaskRunResourcesOutput(anotherCloudEventResource.Name, tb.TaskResourceBindingRef(anotherCloudEventResource.Name)),
			),
		),
	)
	taskRunWithTwoCEResourcesInit := tb.TaskRun("test-taskrun-two-ce-resources-init",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(twoOutputsTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesOutput(cloudEventResource.Name, tb.TaskResourceBindingRef(cloudEventResource.Name)),
				tb.TaskRunResourcesOutput(anotherCloudEventResource.Name, tb.TaskResourceBindingRef(anotherCloudEventResource.Name)),
			),
		),
		tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 0, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 0, v1beta1.CloudEventConditionUnknown),
		),
	)
	taskRunWithCESucceded := tb.TaskRun("test-taskrun-ce-succeeded",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSelfLink("/task/1234"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(twoOutputsTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesOutput(cloudEventResource.Name, tb.TaskResourceBindingRef(cloudEventResource.Name)),
				tb.TaskRunResourcesOutput(anotherCloudEventResource.Name, tb.TaskResourceBindingRef(anotherCloudEventResource.Name)),
			),
		),
		tb.TaskRunStatus(
			tb.StatusCondition(apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			}),
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 0, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 0, v1beta1.CloudEventConditionUnknown),
		),
	)
	taskRunWithCEFailed := tb.TaskRun("test-taskrun-ce-failed",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSelfLink("/task/1234"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(twoOutputsTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesOutput(cloudEventResource.Name, tb.TaskResourceBindingRef(cloudEventResource.Name)),
				tb.TaskRunResourcesOutput(anotherCloudEventResource.Name, tb.TaskResourceBindingRef(anotherCloudEventResource.Name)),
			),
		),
		tb.TaskRunStatus(
			tb.StatusCondition(apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionFalse,
			}),
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 0, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 0, v1beta1.CloudEventConditionUnknown),
		),
	)
	taskRunWithCESuccededOneAttempt := tb.TaskRun("test-taskrun-ce-succeeded-one-attempt",
		tb.TaskRunNamespace("foo"),
		tb.TaskRunSelfLink("/task/1234"),
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(twoOutputsTask.Name),
			tb.TaskRunResources(
				tb.TaskRunResourcesOutput(cloudEventResource.Name, tb.TaskResourceBindingRef(cloudEventResource.Name)),
				tb.TaskRunResourcesOutput(anotherCloudEventResource.Name, tb.TaskResourceBindingRef(anotherCloudEventResource.Name)),
			),
		),
		tb.TaskRunStatus(
			tb.StatusCondition(apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			}),
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 1, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "fakemessage", 0, v1beta1.CloudEventConditionUnknown),
		),
	)
	taskruns := []*v1beta1.TaskRun{
		taskRunWithNoCEResources, taskRunWithTwoCEResourcesNoInit,
		taskRunWithTwoCEResourcesInit, taskRunWithCESucceded, taskRunWithCEFailed,
		taskRunWithCESuccededOneAttempt,
	}

	d := test.Data{
		TaskRuns:          taskruns,
		Tasks:             []*v1beta1.Task{simpleTask, twoOutputsTask},
		ClusterTasks:      []*v1beta1.ClusterTask{},
		PipelineResources: []*resourcev1alpha1.PipelineResource{cloudEventResource, anotherCloudEventResource},
	}
	for _, tc := range []struct {
		name            string
		taskRun         *v1beta1.TaskRun
		wantCloudEvents []v1beta1.CloudEventDelivery
	}{{
		name:            "no-ce-resources",
		taskRun:         taskRunWithNoCEResources,
		wantCloudEvents: taskRunWithNoCEResources.Status.CloudEvents,
	}, {
		name:    "ce-resources-no-init",
		taskRun: taskRunWithTwoCEResourcesNoInit,
		wantCloudEvents: tb.TaskRun("want", tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 0, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 0, v1beta1.CloudEventConditionUnknown),
		)).Status.CloudEvents,
	}, {
		name:    "ce-resources-init",
		taskRun: taskRunWithTwoCEResourcesInit,
		wantCloudEvents: tb.TaskRun("want2", tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 0, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 0, v1beta1.CloudEventConditionUnknown),
		)).Status.CloudEvents,
	}, {
		name:    "ce-resources-init-task-successful",
		taskRun: taskRunWithCESucceded,
		wantCloudEvents: tb.TaskRun("want3", tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 1, v1beta1.CloudEventConditionSent),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 1, v1beta1.CloudEventConditionSent),
		)).Status.CloudEvents,
	}, {
		name:    "ce-resources-init-task-failed",
		taskRun: taskRunWithCEFailed,
		wantCloudEvents: tb.TaskRun("want4", tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 1, v1beta1.CloudEventConditionSent),
			tb.TaskRunCloudEvent(cloudEventTarget2, "", 1, v1beta1.CloudEventConditionSent),
		)).Status.CloudEvents,
	}, {
		name:    "ce-resources-init-task-successful-one-attempt",
		taskRun: taskRunWithCESuccededOneAttempt,
		wantCloudEvents: tb.TaskRun("want5", tb.TaskRunStatus(
			tb.TaskRunCloudEvent(cloudEventTarget1, "", 1, v1beta1.CloudEventConditionUnknown),
			tb.TaskRunCloudEvent(cloudEventTarget2, "fakemessage", 1, v1beta1.CloudEventConditionSent),
		)).Status.CloudEvents,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			names.TestingSeed()
			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()
			c := testAssets.Controller
			clients := testAssets.Clients

			saName := tc.taskRun.Spec.ServiceAccountName
			if saName == "" {
				saName = "default"
			}
			if _, err := clients.Kube.CoreV1().ServiceAccounts(tc.taskRun.Namespace).Create(&corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      saName,
					Namespace: tc.taskRun.Namespace,
				},
			}); err != nil {
				t.Fatal(err)
			}

			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tc.taskRun)); err != nil {
				t.Errorf("expected no error. Got error %v", err)
			}

			tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tc.taskRun.Namespace).Get(tc.taskRun.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("getting updated taskrun: %v", err)
			}
			opts := cloudevent.GetCloudEventDeliveryCompareOptions()
			t.Log(tr.Status.CloudEvents)
			if d := cmp.Diff(tc.wantCloudEvents, tr.Status.CloudEvents, opts...); d != "" {
				t.Errorf("Unexpected status of cloud events %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResourceResult(t *testing.T) {
	for _, c := range []struct {
		desc          string
		pod           corev1.Pod
		taskRunStatus *v1beta1.TaskRunStatus
		want          []resourcev1alpha1.PipelineResourceResult
	}{{
		desc: "image resource updated",
		pod: corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `[{"key":"digest","value":"sha256:1234","resourceRef":{"name":"source-image"}}]`,
						},
					},
				}},
			},
		},
		want: []resourcev1alpha1.PipelineResourceResult{{
			Key:         "digest",
			Value:       "sha256:1234",
			ResourceRef: resourcev1alpha1.PipelineResourceRef{Name: "source-image"},
		}},
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			tr := &v1beta1.TaskRun{}
			tr.Status.SetCondition(&apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			})
			if err := updateTaskRunResourceResult(tr, c.pod); err != nil {
				t.Errorf("updateTaskRunResourceResult: %s", err)
			}
			if d := cmp.Diff(c.want, tr.Status.ResourcesResult); d != "" {
				t.Errorf("updateTaskRunResourceResult %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResult(t *testing.T) {
	for _, c := range []struct {
		desc          string
		pod           corev1.Pod
		taskRunStatus *v1beta1.TaskRunStatus
		wantResults   []v1beta1.TaskRunResult
		want          []resourcev1alpha1.PipelineResourceResult
	}{{
		desc: "test result with pipeline result",
		pod: corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `[{"key":"resultName","value":"resultValue", "type": "TaskRunResult"}, {"key":"digest","value":"sha256:1234","resourceRef":{"name":"source-image"}, "type": "PipelineResourceResult"}]`,
						},
					},
				}},
			},
		},
		wantResults: []v1beta1.TaskRunResult{{
			Name:  "resultName",
			Value: "resultValue",
		}},
		want: []resourcev1alpha1.PipelineResourceResult{{
			Key:         "digest",
			Value:       "sha256:1234",
			ResourceRef: resourcev1alpha1.PipelineResourceRef{Name: "source-image"},
			ResultType:  "PipelineResourceResult",
		}},
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			tr := &v1beta1.TaskRun{}
			tr.Status.SetCondition(&apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			})
			if err := updateTaskRunResourceResult(tr, c.pod); err != nil {
				t.Errorf("updateTaskRunResourceResult: %s", err)
			}
			if d := cmp.Diff(c.wantResults, tr.Status.TaskRunResults); d != "" {
				t.Errorf("updateTaskRunResourceResult TaskRunResults %s", diff.PrintWantGot(d))
			}
			if d := cmp.Diff(c.want, tr.Status.ResourcesResult); d != "" {
				t.Errorf("updateTaskRunResourceResult ResourcesResult %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResult2(t *testing.T) {
	for _, c := range []struct {
		desc          string
		pod           corev1.Pod
		taskRunStatus *v1beta1.TaskRunStatus
		wantResults   []v1beta1.TaskRunResult
		want          []resourcev1alpha1.PipelineResourceResult
	}{{
		desc: "test result with pipeline result - no result type",
		pod: corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `[{"key":"resultName","value":"resultValue", "type": "TaskRunResult"}, {"key":"digest","value":"sha256:1234","resourceRef":{"name":"source-image"}}]`,
						},
					},
				}},
			},
		},
		wantResults: []v1beta1.TaskRunResult{{
			Name:  "resultName",
			Value: "resultValue",
		}},
		want: []resourcev1alpha1.PipelineResourceResult{{
			Key:         "digest",
			Value:       "sha256:1234",
			ResourceRef: resourcev1alpha1.PipelineResourceRef{Name: "source-image"},
		}},
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			tr := &v1beta1.TaskRun{}
			tr.Status.SetCondition(&apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			})
			if err := updateTaskRunResourceResult(tr, c.pod); err != nil {
				t.Errorf("updateTaskRunResourceResult: %s", err)
			}
			if d := cmp.Diff(c.wantResults, tr.Status.TaskRunResults); d != "" {
				t.Errorf("updateTaskRunResourceResult %s", diff.PrintWantGot(d))
			}
			if d := cmp.Diff(c.want, tr.Status.ResourcesResult); d != "" {
				t.Errorf("updateTaskRunResourceResult %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResultTwoResults(t *testing.T) {
	for _, c := range []struct {
		desc          string
		pod           corev1.Pod
		taskRunStatus *v1beta1.TaskRunStatus
		want          []v1beta1.TaskRunResult
	}{{
		desc: "two test results",
		pod: corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `[{"key":"resultNameOne","value":"resultValueOne", "type": "TaskRunResult"},{"key":"resultNameTwo","value":"resultValueTwo", "type": "TaskRunResult"}]`,
						},
					},
				}, {
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `[{"key":"resultNameOne","value":"resultValueThree", "type": "TaskRunResult"},{"key":"resultNameTwo","value":"resultValueTwo", "type": "TaskRunResult"}]`,
						},
					},
				}},
			},
		},
		want: []v1beta1.TaskRunResult{{
			Name:  "resultNameOne",
			Value: "resultValueThree",
		}, {
			Name:  "resultNameTwo",
			Value: "resultValueTwo",
		}},
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			tr := &v1beta1.TaskRun{}
			tr.Status.SetCondition(&apis.Condition{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			})
			if err := updateTaskRunResourceResult(tr, c.pod); err != nil {
				t.Errorf("updateTaskRunResourceResult: %s", err)
			}
			if d := cmp.Diff(c.want, tr.Status.TaskRunResults); d != "" {
				t.Errorf("updateTaskRunResourceResult %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResultWhenTaskFailed(t *testing.T) {
	for _, c := range []struct {
		desc          string
		podStatus     corev1.PodStatus
		taskRunStatus *v1beta1.TaskRunStatus
		wantResults   []v1beta1.TaskRunResult
		want          []resourcev1alpha1.PipelineResourceResult
	}{{
		desc: "update task results when task fails",
		podStatus: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message: `[{"key":"resultName","value":"resultValue", "type": "TaskRunResult"}, {"name":"source-image","digest":"sha256:1234"}]`,
					},
				},
			}},
		},
		taskRunStatus: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{Conditions: []apis.Condition{{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionFalse,
			}}},
		},
		wantResults: nil,
		want:        nil,
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			if d := cmp.Diff(c.want, c.taskRunStatus.ResourcesResult); d != "" {
				t.Errorf("updateTaskRunResourceResult resources %s", diff.PrintWantGot(d))
			}
			if d := cmp.Diff(c.wantResults, c.taskRunStatus.TaskRunResults); d != "" {
				t.Errorf("updateTaskRunResourceResult results %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestUpdateTaskRunResourceResult_Errors(t *testing.T) {
	for _, c := range []struct {
		desc          string
		pod           corev1.Pod
		taskRunStatus *v1beta1.TaskRunStatus
		want          []resourcev1alpha1.PipelineResourceResult
	}{{
		desc: "image resource exporter with malformed json output",
		pod: corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Message: `MALFORMED JSON{"digest":"sha256:1234"}`,
						},
					},
				}},
			},
		},
		taskRunStatus: &v1beta1.TaskRunStatus{
			Status: duckv1beta1.Status{Conditions: []apis.Condition{{
				Type:   apis.ConditionSucceeded,
				Status: corev1.ConditionTrue,
			}}},
		},
		want: nil,
	}} {
		t.Run(c.desc, func(t *testing.T) {
			names.TestingSeed()
			if err := updateTaskRunResourceResult(&v1beta1.TaskRun{Status: *c.taskRunStatus}, c.pod); err == nil {
				t.Error("Expected error, got nil")
			}
			if d := cmp.Diff(c.want, c.taskRunStatus.ResourcesResult); d != "" {
				t.Errorf("updateTaskRunResourceResult %s", diff.PrintWantGot(d))
			}
		})
	}
}

func TestReconcile_Single_SidecarState(t *testing.T) {
	runningState := corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: time.Now()}}
	taskRun := tb.TaskRun("test-taskrun-sidecars",
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(taskSidecar.Name),
		),
		tb.TaskRunStatus(
			tb.SidecarState(
				tb.SidecarStateName("sidecar"),
				tb.SidecarStateImageID("image-id"),
				tb.SidecarStateContainerName("sidecar-sidecar"),
				tb.SetSidecarStateRunning(runningState),
			),
		),
	)

	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{taskSidecar},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	clients := testAssets.Clients

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	getTaskRun, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	expected := v1beta1.SidecarState{
		Name:          "sidecar",
		ImageID:       "image-id",
		ContainerName: "sidecar-sidecar",
		ContainerState: corev1.ContainerState{
			Running: &runningState,
		},
	}

	if c := cmp.Diff(expected, getTaskRun.Status.Sidecars[0]); c != "" {
		t.Errorf("TestReconcile_Single_SidecarState %s", diff.PrintWantGot(c))
	}
}

func TestReconcile_Multiple_SidecarStates(t *testing.T) {
	runningState := corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: time.Now()}}
	waitingState := corev1.ContainerStateWaiting{Reason: "PodInitializing"}
	taskRun := tb.TaskRun("test-taskrun-sidecars",
		tb.TaskRunSpec(
			tb.TaskRunTaskRef(taskMultipleSidecars.Name),
		),
		tb.TaskRunStatus(
			tb.SidecarState(
				tb.SidecarStateName("sidecar1"),
				tb.SidecarStateImageID("image-id"),
				tb.SidecarStateContainerName("sidecar-sidecar1"),
				tb.SetSidecarStateRunning(runningState),
			),
		),
		tb.TaskRunStatus(
			tb.SidecarState(
				tb.SidecarStateName("sidecar2"),
				tb.SidecarStateImageID("image-id"),
				tb.SidecarStateContainerName("sidecar-sidecar2"),
				tb.SetSidecarStateWaiting(waitingState),
			),
		),
	)

	d := test.Data{
		TaskRuns: []*v1beta1.TaskRun{taskRun},
		Tasks:    []*v1beta1.Task{taskMultipleSidecars},
	}

	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	clients := testAssets.Clients

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	getTaskRun, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected completed TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	expected := []v1beta1.SidecarState{
		{
			Name:          "sidecar1",
			ImageID:       "image-id",
			ContainerName: "sidecar-sidecar1",
			ContainerState: corev1.ContainerState{
				Running: &runningState,
			},
		},
		{
			Name:          "sidecar2",
			ImageID:       "image-id",
			ContainerName: "sidecar-sidecar2",
			ContainerState: corev1.ContainerState{
				Waiting: &waitingState,
			},
		},
	}

	for i, sc := range getTaskRun.Status.Sidecars {
		if c := cmp.Diff(expected[i], sc); c != "" {
			t.Errorf("TestReconcile_Multiple_SidecarStates sidecar%d %s", i+1, diff.PrintWantGot(c))
		}
	}
}

// TestReconcileWorkspaceMissing tests a reconcile of a TaskRun that does
// not include a Workspace that the Task is expecting.
func TestReconcileWorkspaceMissing(t *testing.T) {
	taskWithWorkspace := tb.Task("test-task-with-workspace",
		tb.TaskSpec(
			tb.TaskWorkspace("ws1", "a test task workspace", "", true),
		), tb.TaskNamespace("foo"))
	taskRun := tb.TaskRun("test-taskrun-missing-workspace", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(taskWithWorkspace.Name, tb.TaskRefAPIVersion("a1")),
	))
	d := test.Data{
		Tasks:             []*v1beta1.Task{taskWithWorkspace},
		TaskRuns:          []*v1beta1.TaskRun{taskRun},
		ClusterTasks:      nil,
		PipelineResources: nil,
	}
	names.TestingSeed()
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	clients := testAssets.Clients

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	failedCorrectly := false
	for _, c := range tr.Status.Conditions {
		if c.Type == apis.ConditionSucceeded && c.Status == corev1.ConditionFalse && c.Reason == podconvert.ReasonFailedValidation {
			failedCorrectly = true
		}
	}
	if !failedCorrectly {
		t.Errorf("Expected TaskRun to fail validation but it did not. Final conditions were:\n%#v", tr.Status.Conditions)
	}
}

func TestReconcileTaskResourceResolutionAndValidation(t *testing.T) {
	for _, tt := range []struct {
		desc             string
		d                test.Data
		wantFailedReason string
		wantEvents       []string
	}{{
		desc: "Fail ResolveTaskResources",
		d: test.Data{
			Tasks: []*v1beta1.Task{
				tb.Task("test-task-missing-resource",
					tb.TaskSpec(
						tb.TaskResources(tb.TaskResourcesInput("workspace", resourcev1alpha1.PipelineResourceTypeGit)),
					), tb.TaskNamespace("foo")),
			},
			TaskRuns: []*v1beta1.TaskRun{
				tb.TaskRun("test-taskrun-missing-resource", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
					tb.TaskRunTaskRef("test-task-missing-resource", tb.TaskRefAPIVersion("a1")),
					tb.TaskRunResources(
						tb.TaskRunResourcesInput("workspace", tb.TaskResourceBindingRef("git")),
					),
				)),
			},
			ClusterTasks:      nil,
			PipelineResources: nil,
		},
		wantFailedReason: podconvert.ReasonFailedResolution,
		wantEvents: []string{
			"Normal Started ",
			"Warning Failed",
		},
	}, {
		desc: "Fail ValidateResolvedTaskResources",
		d: test.Data{
			Tasks: []*v1beta1.Task{
				tb.Task("test-task-missing-resource",
					tb.TaskSpec(
						tb.TaskResources(tb.TaskResourcesInput("workspace", resourcev1alpha1.PipelineResourceTypeGit)),
					), tb.TaskNamespace("foo")),
			},
			TaskRuns: []*v1beta1.TaskRun{
				tb.TaskRun("test-taskrun-missing-resource", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
					tb.TaskRunTaskRef("test-task-missing-resource", tb.TaskRefAPIVersion("a1")),
				)),
			},
			ClusterTasks:      nil,
			PipelineResources: nil,
		},
		wantFailedReason: podconvert.ReasonFailedValidation,
		wantEvents: []string{
			"Normal Started ",
			"Warning Failed",
		},
	}} {
		t.Run(tt.desc, func(t *testing.T) {
			names.TestingSeed()
			testAssets, cancel := getTaskRunController(t, tt.d)
			defer cancel()
			clients := testAssets.Clients
			c := testAssets.Controller

			if err := c.Reconciler.Reconcile(context.Background(), getRunName(tt.d.TaskRuns[0])); err != nil {
				t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
			}

			tr, err := clients.Pipeline.TektonV1beta1().TaskRuns(tt.d.TaskRuns[0].Namespace).Get(tt.d.TaskRuns[0].Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Expected TaskRun %s to exist but instead got error when getting it: %v", tt.d.TaskRuns[0].Name, err)
			}

			for _, c := range tr.Status.Conditions {
				if c.Type != apis.ConditionSucceeded || c.Status != corev1.ConditionFalse || c.Reason != tt.wantFailedReason {
					t.Errorf("Expected TaskRun to \"%s\" but it did not. Final conditions were:\n%#v", tt.wantFailedReason, tr.Status.Conditions)
				}
			}

			err = checkEvents(testAssets.Recorder, tt.desc, tt.wantEvents)
			if !(err == nil) {
				t.Errorf(err.Error())
			}
		})
	}
}

// TestReconcileWorkspaceWithVolumeClaimTemplate tests a reconcile of a TaskRun that has
// a Workspace with VolumeClaimTemplate and check that it is translated to a created PersistentVolumeClaim.
func TestReconcileWorkspaceWithVolumeClaimTemplate(t *testing.T) {
	workspaceName := "ws1"
	claimName := "mypvc"
	taskWithWorkspace := tb.Task("test-task-with-workspace", tb.TaskNamespace("foo"),
		tb.TaskSpec(
			tb.TaskWorkspace(workspaceName, "a test task workspace", "", true),
		))
	taskRun := tb.TaskRun("test-taskrun-missing-workspace", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
		tb.TaskRunTaskRef(taskWithWorkspace.Name, tb.TaskRefAPIVersion("a1")),
		tb.TaskRunWorkspaceVolumeClaimTemplate(workspaceName, "", &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: claimName,
			},
			Spec: corev1.PersistentVolumeClaimSpec{},
		}),
	))
	d := test.Data{
		Tasks:             []*v1beta1.Task{taskWithWorkspace},
		TaskRuns:          []*v1beta1.TaskRun{taskRun},
		ClusterTasks:      nil,
		PipelineResources: nil,
	}
	names.TestingSeed()
	testAssets, cancel := getTaskRunController(t, d)
	defer cancel()
	clients := testAssets.Clients

	if err := testAssets.Controller.Reconciler.Reconcile(context.Background(), getRunName(taskRun)); err != nil {
		t.Errorf("expected no error reconciling valid TaskRun but got %v", err)
	}

	ttt, err := clients.Pipeline.TektonV1beta1().TaskRuns(taskRun.Namespace).Get(taskRun.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected TaskRun %s to exist but instead got error when getting it: %v", taskRun.Name, err)
	}

	for _, w := range ttt.Spec.Workspaces {
		if w.PersistentVolumeClaim != nil {
			t.Fatalf("expected workspace from volumeClaimTemplate to be translated to PVC")
		}
	}

	expectedPVCName := fmt.Sprintf("%s-%s-%s", claimName, workspaceName, taskRun.Name)
	_, err = clients.Kube.CoreV1().PersistentVolumeClaims(taskRun.Namespace).Get(expectedPVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected PVC %s to exist but instead got error when getting it: %v", expectedPVCName, err)
	}
}

func TestFailTaskRun(t *testing.T) {
	testCases := []struct {
		name           string
		taskRun        *v1beta1.TaskRun
		pod            *corev1.Pod
		reason         v1beta1.TaskRunReason
		message        string
		expectedStatus apis.Condition
	}{{
		name: "no-pod-scheduled",
		taskRun: tb.TaskRun("test-taskrun-run-failed", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
			tb.TaskRunCancelled,
		), tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: corev1.ConditionUnknown,
		}))),
		reason:  "some reason",
		message: "some message",
		expectedStatus: apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  "some reason",
			Message: "some message",
		},
	}, {
		name: "pod-scheduled",
		taskRun: tb.TaskRun("test-taskrun-run-failed", tb.TaskRunNamespace("foo"), tb.TaskRunSpec(
			tb.TaskRunTaskRef(simpleTask.Name),
			tb.TaskRunCancelled,
		), tb.TaskRunStatus(tb.StatusCondition(apis.Condition{
			Type:   apis.ConditionSucceeded,
			Status: corev1.ConditionUnknown,
		}), tb.PodName("foo-is-bar"))),
		pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "foo",
			Name:      "foo-is-bar",
		}},
		reason:  "some reason",
		message: "some message",
		expectedStatus: apis.Condition{
			Type:    apis.ConditionSucceeded,
			Status:  corev1.ConditionFalse,
			Reason:  "some reason",
			Message: "some message",
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			d := test.Data{
				TaskRuns: []*v1beta1.TaskRun{tc.taskRun},
			}
			if tc.pod != nil {
				d.Pods = []*corev1.Pod{tc.pod}
			}

			testAssets, cancel := getTaskRunController(t, d)
			defer cancel()

			// Use the test assets to create a *Reconciler directly for focused testing.
			c := &Reconciler{
				KubeClientSet:     testAssets.Clients.Kube,
				PipelineClientSet: testAssets.Clients.Pipeline,
				taskRunLister:     testAssets.Informers.TaskRun.Lister(),
				taskLister:        testAssets.Informers.Task.Lister(),
				clusterTaskLister: testAssets.Informers.ClusterTask.Lister(),
				resourceLister:    testAssets.Informers.PipelineResource.Lister(),
				timeoutHandler:    nil, // Not used
				cloudEventClient:  testAssets.Clients.CloudEvents,
				metrics:           nil, // Not used
				entrypointCache:   nil, // Not used
				pvcHandler:        volumeclaim.NewPVCHandler(testAssets.Clients.Kube, testAssets.Logger),
			}

			err := c.failTaskRun(context.Background(), tc.taskRun, tc.reason, tc.message)
			if err != nil {
				t.Fatal(err)
			}
			if d := cmp.Diff(tc.taskRun.Status.GetCondition(apis.ConditionSucceeded), &tc.expectedStatus, ignoreLastTransitionTime); d != "" {
				t.Fatalf(diff.PrintWantGot(d))
			}
		})
	}
}

func Test_storeTaskSpec(t *testing.T) {

	ctx := context.Background()
	tr := tb.TaskRun("foo", tb.TaskRunSpec(tb.TaskRunTaskRef("foo-task")))

	ts := tb.Task("some-task", tb.TaskSpec(tb.TaskDescription("foo-task"))).Spec
	ts1 := tb.Task("some-task", tb.TaskSpec(tb.TaskDescription("foo-task"))).Spec
	want := ts.DeepCopy()

	// The first time we set it, it should get copied.
	if err := storeTaskSpec(ctx, tr, &ts); err != nil {
		t.Errorf("storeTaskSpec() error = %v", err)
	}
	if d := cmp.Diff(tr.Status.TaskSpec, want); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}

	// The next time, it should not get overwritten
	if err := storeTaskSpec(ctx, tr, &ts1); err != nil {
		t.Errorf("storeTaskSpec() error = %v", err)
	}
	if d := cmp.Diff(tr.Status.TaskSpec, want); d != "" {
		t.Fatalf(diff.PrintWantGot(d))
	}
}
