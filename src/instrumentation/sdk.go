/*
Copyright 2024.

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

package instrumentation

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.5.0"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/newrelic/k8s-agents-operator/src/api/v1alpha1"
	apm "github.com/newrelic/k8s-agents-operator/src/apm"
	"github.com/newrelic/k8s-agents-operator/src/constants"
)

type sdkInjector struct {
	client client.Client
	logger logr.Logger
}

func (i *sdkInjector) inject(ctx context.Context, insts languageInstrumentations, ns corev1.Namespace, pod corev1.Pod, containerName string) corev1.Pod {
	if len(pod.Spec.Containers) < 1 {
		return pod
	}

	// We search for specific container to inject variables and if no one is found
	// We fallback to first container
	var index = 0
	for idx, ctnair := range pod.Spec.Containers {
		if ctnair.Name == containerName {
			index = idx
		}
	}

	if insts.Java != nil {
		newrelic := *insts.Java
		var err error
		i.logger.V(1).Info("injecting Java instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)
		pod, err = apm.InjectJavaagent(newrelic.Spec.Java, pod, index)
		if err != nil {
			i.logger.Info("Skipping Java agent injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			pod = i.injectNewrelicConfig(ctx, newrelic, ns, pod, index)
		}
	}
	if insts.NodeJS != nil {
		newrelic := *insts.NodeJS
		var err error
		i.logger.V(1).Info("injecting NodeJS instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)
		pod, err = apm.InjectNodeJSSDK(newrelic.Spec.NodeJS, pod, index)
		if err != nil {
			i.logger.Info("Skipping NodeJS agent injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			pod = i.injectNewrelicConfig(ctx, newrelic, ns, pod, index)
		}
	}
	if insts.Python != nil {
		newrelic := *insts.Python
		var err error
		i.logger.V(1).Info("injecting Python instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)
		pod, err = apm.InjectPythonSDK(newrelic.Spec.Python, pod, index)
		if err != nil {
			i.logger.Info("Skipping Python agent injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			pod = i.injectNewrelicConfig(ctx, newrelic, ns, pod, index)
		}
	}
	if insts.DotNet != nil {
		newrelic := *insts.DotNet
		var err error
		i.logger.V(1).Info("injecting DotNet instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)
		pod, err = apm.InjectDotNetSDK(newrelic.Spec.DotNet, pod, index)
		if err != nil {
			i.logger.Info("Skipping DotNet agent injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			pod = i.injectNewrelicConfig(ctx, newrelic, ns, pod, index)
		}
	}
	if insts.Php != nil {
		newrelic := *insts.Php
		var err error
		i.logger.V(1).Info("injecting Php instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)
		pod, err = apm.InjectPhpagent(newrelic.Spec.Php, pod, index)
		if err != nil {
			i.logger.Info("Skipping Php agent injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			pod = i.injectNewrelicConfig(ctx, newrelic, ns, pod, index)
		}
	}
	if insts.Go != nil {
		newrelic := *insts.Go
		var err error
		i.logger.V(1).Info("injecting Go instrumentation into pod", "newrelic-namespace", newrelic.Namespace, "newrelic-name", newrelic.Name)

		goContainers := annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationInjectGoContainerName)
		index := getContainerIndex(goContainers, pod)

		// Go instrumentation supports only single container instrumentation.
		pod, err = apm.InjectGoSDK(newrelic.Spec.Go, pod)
		if err != nil {
			i.logger.Info("Skipping Go SDK injection", "reason", err.Error(), "container", pod.Spec.Containers[index].Name)
		} else {
			// Common env vars and config need to be applied to the agent container.
			pod = i.injectCommonEnvVar(newrelic, pod, len(pod.Spec.Containers)-1)
			pod = i.injectCommonSDKConfig(ctx, newrelic, ns, pod, len(pod.Spec.Containers)-1, 0)
		}
	}
	return pod
}

func getContainerIndex(containerName string, pod corev1.Pod) int {
	// We search for specific container to inject variables and if no one is found
	// We fallback to first container
	var index = 0
	for idx, ctnair := range pod.Spec.Containers {
		if ctnair.Name == containerName {
			index = idx
		}
	}

	return index
}

func (i *sdkInjector) injectCommonEnvVar(newrelic v1alpha1.Instrumentation, pod corev1.Pod, index int) corev1.Pod {
	container := &pod.Spec.Containers[index]
	for _, env := range newrelic.Spec.Env {
		idx := getIndexOfEnv(container.Env, env.Name)
		if idx == -1 {
			container.Env = append(container.Env, env)
		}
	}
	return pod
}

// injectCommonSDKConfig adds common SDK configuration environment variables to the necessary pod
// agentIndex represents the index of the pod the needs the env vars to instrument the application.
// appIndex represents the index of the pod the will produce the telemetry.
// When the pod handling the instrumentation is the same as the pod producing the telemetry agentIndex
// and appIndex should be the same value.  This is true for dotnet, java, nodejs, and python instrumentations.
// Go requires the agent to be a different container in the pod, so the agentIndex should represent this new sidecar
// and appIndex should represent the application being instrumented.
func (i *sdkInjector) injectCommonSDKConfig(ctx context.Context, newrelic v1alpha1.Instrumentation, ns corev1.Namespace, pod corev1.Pod, agentIndex int, appIndex int) corev1.Pod {
	container := &pod.Spec.Containers[agentIndex]
	resourceMap := i.createResourceMap(ctx, newrelic, ns, pod, appIndex)
	idx := getIndexOfEnv(container.Env, constants.EnvOTELServiceName)
	if idx == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvOTELServiceName,
			Value: chooseServiceName(pod, resourceMap, appIndex),
		})
	}
	if newrelic.Spec.Exporter.Endpoint != "" {
		idx = getIndexOfEnv(container.Env, constants.EnvOTELExporterOTLPEndpoint)
		if idx == -1 {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  constants.EnvOTELExporterOTLPEndpoint,
				Value: newrelic.Spec.Endpoint,
			})
		}
	}

	// Some attributes might be empty, we should get them via k8s downward API
	if resourceMap[string(semconv.K8SPodNameKey)] == "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: constants.EnvPodName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		})
		resourceMap[string(semconv.K8SPodNameKey)] = fmt.Sprintf("$(%s)", constants.EnvPodName)
	}
	if newrelic.Spec.Resource.AddK8sUIDAttributes {
		if resourceMap[string(semconv.K8SPodUIDKey)] == "" {
			container.Env = append(container.Env, corev1.EnvVar{
				Name: constants.EnvPodUID,
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.uid",
					},
				},
			})
			resourceMap[string(semconv.K8SPodUIDKey)] = fmt.Sprintf("$(%s)", constants.EnvPodUID)
		}
	}

	idx = getIndexOfEnv(container.Env, constants.EnvOTELResourceAttrs)
	if idx == -1 || !strings.Contains(container.Env[idx].Value, string(semconv.ServiceVersionKey)) {
		vsn := chooseServiceVersion(pod, appIndex)
		if vsn != "" {
			resourceMap[string(semconv.ServiceVersionKey)] = vsn
		}
	}

	if resourceMap[string(semconv.K8SNodeNameKey)] == "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name: constants.EnvNodeName,
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "spec.nodeName",
				},
			},
		})
		resourceMap[string(semconv.K8SNodeNameKey)] = fmt.Sprintf("$(%s)", constants.EnvNodeName)
	}

	idx = getIndexOfEnv(container.Env, constants.EnvOTELResourceAttrs)
	resStr := resourceMapToStr(resourceMap)
	if idx == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvOTELResourceAttrs,
			Value: resStr,
		})
	} else {
		if !strings.HasSuffix(container.Env[idx].Value, ",") {
			resStr = "," + resStr
		}
		container.Env[idx].Value += resStr
	}

	idx = getIndexOfEnv(container.Env, constants.EnvOTELPropagators)
	if idx == -1 && len(newrelic.Spec.Propagators) > 0 {
		propagators := *(*[]string)((unsafe.Pointer(&newrelic.Spec.Propagators)))
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvOTELPropagators,
			Value: strings.Join(propagators, ","),
		})
	}

	idx = getIndexOfEnv(container.Env, constants.EnvOTELTracesSampler)
	// configure sampler only if it is configured in the CR
	if idx == -1 && newrelic.Spec.Sampler.Type != "" {
		idxSamplerArg := getIndexOfEnv(container.Env, constants.EnvOTELTracesSamplerArg)
		if idxSamplerArg == -1 {
			container.Env = append(container.Env, corev1.EnvVar{
				Name:  constants.EnvOTELTracesSampler,
				Value: string(newrelic.Spec.Sampler.Type),
			})
			if newrelic.Spec.Sampler.Argument != "" {
				container.Env = append(container.Env, corev1.EnvVar{
					Name:  constants.EnvOTELTracesSamplerArg,
					Value: newrelic.Spec.Sampler.Argument,
				})
			}
		}
	}

	// Move OTEL_RESOURCE_ATTRIBUTES to last position on env list.
	// When OTEL_RESOURCE_ATTRIBUTES environment variable uses other env vars
	// as attributes value they have to be configured before.
	// It is mandatory to set right order to avoid attributes with value
	// pointing to the name of used environment variable instead of its value.
	idx = getIndexOfEnv(container.Env, constants.EnvOTELResourceAttrs)
	envs := moveEnvToListEnd(container.Env, idx)
	container.Env = envs

	return pod
}

func (i *sdkInjector) injectNewrelicConfig(ctx context.Context, newrelic v1alpha1.Instrumentation, ns corev1.Namespace, pod corev1.Pod, index int) corev1.Pod {
	container := &pod.Spec.Containers[index]
	resourceMap := i.createResourceMap(ctx, newrelic, ns, pod, index)
	idx := getIndexOfEnv(container.Env, constants.EnvNewRelicAppName)
	if idx == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  constants.EnvNewRelicAppName,
			Value: chooseServiceName(pod, resourceMap, index),
		})
	}
	idx = getIndexOfEnv(container.Env, constants.EnvNewRelicLicenseKey)
	if idx == -1 {
		optional := true
		container.Env = append(container.Env, corev1.EnvVar{
			Name: constants.EnvNewRelicLicenseKey,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "newrelic-key-secret"},
					Key:                  "new_relic_license_key",
					Optional:             &optional,
				},
			},
		})
	}
	idx = getIndexOfEnv(container.Env, constants.EnvNewRelicLabels)
	if idx == -1 {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "NEW_RELIC_LABELS",
			Value: "operator:auto-injection",
		})
	}
	return pod
}

func chooseServiceName(pod corev1.Pod, resources map[string]string, index int) string {
	if name := resources[string(semconv.K8SDeploymentNameKey)]; name != "" {
		return name
	}
	if name := resources[string(semconv.K8SStatefulSetNameKey)]; name != "" {
		return name
	}
	if name := resources[string(semconv.K8SJobNameKey)]; name != "" {
		return name
	}
	if name := resources[string(semconv.K8SCronJobNameKey)]; name != "" {
		return name
	}
	if name := resources[string(semconv.K8SPodNameKey)]; name != "" {
		return name
	}
	return pod.Spec.Containers[index].Name
}

// obtains version by splitting image string on ":" and extracting final element from resulting array.
func chooseServiceVersion(pod corev1.Pod, index int) string {
	parts := strings.Split(pod.Spec.Containers[index].Image, ":")
	tag := parts[len(parts)-1]
	//guard statement to handle case where image name has a port number
	if strings.Contains(tag, "/") {
		return ""
	}
	return tag
}

func resourceMapToStr(res map[string]string) string {
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var str = ""
	for _, k := range keys {
		if str != "" {
			str += ","
		}
		str += fmt.Sprintf("%s=%s", k, res[k])
	}

	return str
}

// creates the service.instance.id following the semantic defined by
// https://github.com/open-telemetry/semantic-conventions/pull/312.
func createServiceInstanceId(namespaceName, podName, containerName string) string {
	var serviceInstanceId string
	if namespaceName != "" && podName != "" && containerName != "" {
		resNames := []string{namespaceName, podName, containerName}
		serviceInstanceId = strings.Join(resNames, ".")
	}
	return serviceInstanceId
}

// createResourceMap creates resource attribute map.
// User defined attributes (in explicitly set env var) have higher precedence.
func (i *sdkInjector) createResourceMap(ctx context.Context, newrelic v1alpha1.Instrumentation, ns corev1.Namespace, pod corev1.Pod, index int) map[string]string {
	// get existing resources env var and parse it into a map
	existingRes := map[string]bool{}
	existingResourceEnvIdx := getIndexOfEnv(pod.Spec.Containers[index].Env, constants.EnvOTELResourceAttrs)
	if existingResourceEnvIdx > -1 {
		existingResArr := strings.Split(pod.Spec.Containers[index].Env[existingResourceEnvIdx].Value, ",")
		for _, kv := range existingResArr {
			keyValueArr := strings.Split(strings.TrimSpace(kv), "=")
			if len(keyValueArr) != 2 {
				continue
			}
			existingRes[keyValueArr[0]] = true
		}
	}

	res := map[string]string{}
	for k, v := range newrelic.Spec.Resource.Attributes {
		if !existingRes[k] {
			res[k] = v
		}
	}
	k8sResources := map[attribute.Key]string{}
	k8sResources[semconv.K8SNamespaceNameKey] = ns.Name
	k8sResources[semconv.K8SContainerNameKey] = pod.Spec.Containers[index].Name
	// Some fields might be empty - node name, pod name
	// The pod name might be empty if the pod is created form deployment template
	k8sResources[semconv.K8SPodNameKey] = pod.Name
	k8sResources[semconv.K8SPodUIDKey] = string(pod.UID)
	k8sResources[semconv.K8SNodeNameKey] = pod.Spec.NodeName
	k8sResources[semconv.ServiceInstanceIDKey] = createServiceInstanceId(ns.Name, pod.Name, pod.Spec.Containers[index].Name)
	i.addParentResourceLabels(ctx, newrelic.Spec.Resource.AddK8sUIDAttributes, ns, pod.ObjectMeta, k8sResources)
	for k, v := range k8sResources {
		if !existingRes[string(k)] && v != "" {
			res[string(k)] = v
		}
	}
	return res
}

func (i *sdkInjector) addParentResourceLabels(ctx context.Context, uid bool, ns corev1.Namespace, objectMeta metav1.ObjectMeta, resources map[attribute.Key]string) {
	for _, owner := range objectMeta.OwnerReferences {
		switch strings.ToLower(owner.Kind) {
		case "replicaset":
			resources[semconv.K8SReplicaSetNameKey] = owner.Name
			if uid {
				resources[semconv.K8SReplicaSetUIDKey] = string(owner.UID)
			}
			// parent of ReplicaSet is e.g. Deployment which we are interested to know
			rs := appsv1.ReplicaSet{}
			nsn := types.NamespacedName{Namespace: ns.Name, Name: owner.Name}
			backOff := wait.Backoff{Duration: 10 * time.Millisecond, Factor: 1.5, Jitter: 0.1, Steps: 20, Cap: 2 * time.Second}

			checkError := func(err error) bool {
				return apierrors.IsNotFound(err)
			}

			getReplicaSet := func() error {
				return i.client.Get(ctx, nsn, &rs)
			}

			// use a retry loop to get the Deployment. A single call to client.get fails occasionally
			err := retry.OnError(backOff, checkError, getReplicaSet)
			if err != nil {
				i.logger.Error(err, "failed to get replicaset", "replicaset", nsn.Name, "namespace", nsn.Namespace)
			}
			i.addParentResourceLabels(ctx, uid, ns, rs.ObjectMeta, resources)
		case "deployment":
			resources[semconv.K8SDeploymentNameKey] = owner.Name
			if uid {
				resources[semconv.K8SDeploymentUIDKey] = string(owner.UID)
			}
		case "statefulset":
			resources[semconv.K8SStatefulSetNameKey] = owner.Name
			if uid {
				resources[semconv.K8SStatefulSetUIDKey] = string(owner.UID)
			}
		case "daemonset":
			resources[semconv.K8SDaemonSetNameKey] = owner.Name
			if uid {
				resources[semconv.K8SDaemonSetUIDKey] = string(owner.UID)
			}
		case "job":
			resources[semconv.K8SJobNameKey] = owner.Name
			if uid {
				resources[semconv.K8SJobUIDKey] = string(owner.UID)
			}
		case "cronjob":
			resources[semconv.K8SCronJobNameKey] = owner.Name
			if uid {
				resources[semconv.K8SCronJobUIDKey] = string(owner.UID)
			}
		}
	}
}

func getIndexOfEnv(envs []corev1.EnvVar, name string) int {
	for i := range envs {
		if envs[i].Name == name {
			return i
		}
	}
	return -1
}

func moveEnvToListEnd(envs []corev1.EnvVar, idx int) []corev1.EnvVar {
	if idx >= 0 && idx < len(envs) {
		envToMove := envs[idx]
		envs = append(envs[:idx], envs[idx+1:]...)
		envs = append(envs, envToMove)
	}

	return envs
}
