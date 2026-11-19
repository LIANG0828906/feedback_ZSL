package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"driftconverge/internal/model"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var ErrNotFound = errors.New("deployment not found")

type Client interface {
	ListDeployments(ctx context.Context, namespace string) ([]model.DeploymentState, error)
	GetDeployment(ctx context.Context, namespace, name string) (model.DeploymentState, error)
	ApplyDeployment(ctx context.Context, desired model.DesiredDeployment) (string, error)
}

type KubeClient struct {
	clientset kubernetes.Interface
}

func New(kubeconfig string) (*KubeClient, error) {
	var (
		config *rest.Config
		err    error
	)
	if kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if env := os.Getenv("KUBECONFIG"); env != "" {
			loadingRules.ExplicitPath = env
		}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules,
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	config.QPS = 5
	config.Burst = 10
	config.Timeout = 5 * time.Second
	if value := os.Getenv("DRIFT_CLUSTER_TIMEOUT"); value != "" {
		if parsed, parseErr := time.ParseDuration(value); parseErr == nil && parsed > 0 {
			config.Timeout = parsed
		}
	}
	config.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	return &KubeClient{clientset: clientset}, nil
}

func NewFromInterface(clientset kubernetes.Interface) *KubeClient {
	return &KubeClient{clientset: clientset}
}

func (c *KubeClient) ListDeployments(ctx context.Context, namespace string) ([]model.DeploymentState, error) {
	list, err := c.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]model.DeploymentState, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, toState(namespace, &list.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *KubeClient) GetDeployment(ctx context.Context, namespace, name string) (model.DeploymentState, error) {
	deployment, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return model.DeploymentState{}, ErrNotFound
	}
	if err != nil {
		return model.DeploymentState{}, err
	}
	return toState(namespace, deployment), nil
}

func (c *KubeClient) ApplyDeployment(ctx context.Context, desired model.DesiredDeployment) (string, error) {
	deployments := c.clientset.AppsV1().Deployments(desired.Namespace)
	current, err := deployments.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, buildErr := model.RawToDeployment(desired.Raw)
		if buildErr != nil {
			return "failed", fmt.Errorf("build Deployment %s: %w", desired.ResourceKey, buildErr)
		}
		if _, createErr := deployments.Create(ctx, created, metav1.CreateOptions{}); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				return "updated", c.updateExisting(ctx, desired)
			}
			return "create_failed", createErr
		}
		return "created", nil
	}
	if err != nil {
		return "get_failed", err
	}
	if err := c.updateCurrent(ctx, desired, current); err != nil {
		return "update_failed", err
	}
	return "updated", nil
}

func (c *KubeClient) updateExisting(ctx context.Context, desired model.DesiredDeployment) error {
	deployments := c.clientset.AppsV1().Deployments(desired.Namespace)
	current, err := deployments.Get(ctx, desired.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	return c.updateCurrent(ctx, desired, current)
}

func (c *KubeClient) updateCurrent(ctx context.Context, desired model.DesiredDeployment, current *appsv1.Deployment) error {
	templateDeployment, err := model.RawToDeployment(desired.Raw)
	if err != nil {
		return fmt.Errorf("decode desired manifest: %w", err)
	}
	desiredContainers := make(map[string]corev1.Container, len(templateDeployment.Spec.Template.Spec.Containers))
	for _, container := range templateDeployment.Spec.Template.Spec.Containers {
		desiredContainers[container.Name] = container
	}

	if current.Spec.Replicas == nil || *current.Spec.Replicas != desired.Replicas {
		current.Spec.Replicas = pointerInt32(desired.Replicas)
	}
	existingByName := make(map[string]int, len(current.Spec.Template.Spec.Containers))
	for index := range current.Spec.Template.Spec.Containers {
		existingByName[current.Spec.Template.Spec.Containers[index].Name] = index
	}
	for name, expected := range desiredContainers {
		if index, found := existingByName[name]; found {
			existing := &current.Spec.Template.Spec.Containers[index]
			if existing.Image != expected.Image {
				existing.Image = expected.Image
			}
			if merged, changed := mergeEnv(existing.Env, expected.Env); changed {
				existing.Env = merged
			}
			continue
		}
		current.Spec.Template.Spec.Containers = append(
			current.Spec.Template.Spec.Containers,
			*expected.DeepCopy(),
		)
	}
	deployments := c.clientset.AppsV1().Deployments(desired.Namespace)
	_, err = deployments.Update(ctx, current, metav1.UpdateOptions{})
	return err
}

func mergeEnv(current, expected []corev1.EnvVar) ([]corev1.EnvVar, bool) {
	if envEqual(current, expected) {
		return current, false
	}
	expectedByName := make(map[string]corev1.EnvVar, len(expected))
	for _, item := range expected {
		expectedByName[item.Name] = item
	}
	merged := make([]corev1.EnvVar, 0, len(expected))
	seen := make(map[string]bool, len(expected))
	for _, item := range current {
		expectedItem, found := expectedByName[item.Name]
		if !found {
			continue
		}
		merged = append(merged, corev1.EnvVar{Name: expectedItem.Name, Value: expectedItem.Value})
		seen[item.Name] = true
	}
	for _, item := range expected {
		if seen[item.Name] {
			continue
		}
		merged = append(merged, corev1.EnvVar{Name: item.Name, Value: item.Value})
	}
	return merged, true
}

func envEqual(current, expected []corev1.EnvVar) bool {
	if len(current) != len(expected) {
		return false
	}
	expectedByName := make(map[string]corev1.EnvVar, len(expected))
	for _, item := range expected {
		expectedByName[item.Name] = item
	}
	for _, item := range current {
		expectedItem, found := expectedByName[item.Name]
		if !found || item.Value != expectedItem.Value || item.ValueFrom != nil || expectedItem.ValueFrom != nil {
			return false
		}
	}
	return true
}

func pointerInt32(value int32) *int32 { return &value }

func toState(namespace string, deployment *appsv1.Deployment) model.DeploymentState {
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	state := model.DeploymentState{
		Namespace:  namespace,
		Name:       deployment.Name,
		Generation: deployment.Generation,
		Replicas:   replicas,
		Containers: make([]model.ContainerState, 0, len(deployment.Spec.Template.Spec.Containers)),
		Status: model.DeploymentStatus{
			ObservedGeneration:  deployment.Status.ObservedGeneration,
			Replicas:            deployment.Status.Replicas,
			UpdatedReplicas:     deployment.Status.UpdatedReplicas,
			ReadyReplicas:       deployment.Status.ReadyReplicas,
			AvailableReplicas:   deployment.Status.AvailableReplicas,
			UnavailableReplicas: deployment.Status.UnavailableReplicas,
		},
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		env := make(map[string]string, len(container.Env))
		for _, item := range container.Env {
			if item.ValueFrom != nil {
				env[item.Name] = "<valueFrom>"
			} else {
				env[item.Name] = item.Value
			}
		}
		state.Containers = append(state.Containers, model.ContainerState{
			Name:  container.Name,
			Image: container.Image,
			Env:   env,
		})
	}
	for _, condition := range deployment.Status.Conditions {
		state.Status.Conditions = append(state.Status.Conditions, model.DeploymentCondition{
			Type:    string(condition.Type),
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
		})
	}
	sort.Slice(state.Containers, func(i, j int) bool { return state.Containers[i].Name < state.Containers[j].Name })
	return state
}
