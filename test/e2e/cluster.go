package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/k3s"
)

const clusterImage = "rancher/k3s:v1.31.4-k3s1"

const (
	clusterStartTimeout = 5 * time.Minute
	fixtureTimeout      = 3 * time.Minute
)

const (
	fixtureNamespace = "e2e-workloads"
	fixtureWorkload  = "settled"
	fixtureImage     = "registry.k8s.io/pause:3.9"
)

const (
	talkativeWorkload = "talkative"
	crashingWorkload  = "crashing"
	logImage          = "busybox:1.36"
	livingMarker      = "e2e-living-container-said-this"
	dyingMarker       = "e2e-dying-container-said-this"
)

const (
	leakingWorkload = "leaking"
	leakedSecret    = "AKIAIOSFODNN7EXAMPLE"
	keptAlongside   = "e2e-line-that-must-survive-redaction"
)

type cluster struct {
	container      *k3s.K3sContainer
	kubeconfigPath string
	client         *kubernetes.Clientset
}

func startCluster(ctx context.Context, workDir string) (*cluster, error) {
	startCtx, cancel := context.WithTimeout(ctx, clusterStartTimeout)
	defer cancel()

	container, err := k3s.Run(startCtx, clusterImage)
	if err != nil {
		return nil, fmt.Errorf("starting kubernetes: %w", err)
	}

	started, err := configureCluster(startCtx, container, workDir)
	if err != nil {
		_ = testcontainers.TerminateContainer(container)
		return nil, err
	}
	return started, nil
}

func configureCluster(
	ctx context.Context, container *k3s.K3sContainer, workDir string,
) (*cluster, error) {
	kubeconfig, err := container.GetKubeConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig: %w", err)
	}

	path := filepath.Join(workDir, "kubeconfig")
	if err = os.WriteFile(path, kubeconfig, 0o600); err != nil {
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}

	client, err := clientFor(kubeconfig)
	if err != nil {
		return nil, err
	}
	return &cluster{container: container, kubeconfigPath: path, client: client}, nil
}

func clientFor(kubeconfig []byte) (*kubernetes.Clientset, error) {
	config, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig the cluster reported: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("building a client from the cluster's kubeconfig: %w", err)
	}
	return client, nil
}

func (c *cluster) close() {
	if c == nil {
		return
	}
	_ = testcontainers.TerminateContainer(c.container)
}

func (c *cluster) createFixture(ctx context.Context) error {
	createCtx, cancel := context.WithTimeout(ctx, fixtureTimeout)
	defer cancel()

	namespaces := c.client.CoreV1().Namespaces()
	_, err := namespaces.Create(createCtx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fixtureNamespace}},
		metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating namespace %s: %w", fixtureNamespace, err)
	}

	labels := map[string]string{"app": fixtureWorkload}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: fixtureWorkload, Namespace: fixtureNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "settled",
					Image: fixtureImage,
				}}},
			},
		},
	}
	if _, err = c.client.AppsV1().Deployments(fixtureNamespace).
		Create(createCtx, deployment, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating deployment %s: %w", fixtureWorkload, err)
	}

	if err = c.createTalker(createCtx, talkativeWorkload,
		"echo "+livingMarker+"; sleep 3600"); err != nil {
		return err
	}
	if err = c.createTalker(createCtx, crashingWorkload,
		"echo "+dyingMarker+"; exit 1"); err != nil {
		return err
	}
	if err = c.createTalker(createCtx, leakingWorkload,
		"echo "+keptAlongside+" aws_access_key_id="+leakedSecret+"; sleep 3600"); err != nil {
		return err
	}
	return c.awaitSettled(createCtx)
}

func (c *cluster) createTalker(ctx context.Context, name, command string) error {
	labels := map[string]string{"app": name}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fixtureNamespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:    name,
					Image:   logImage,
					Command: []string{"sh", "-c", command},
				}}},
			},
		},
	}
	if _, err := c.client.AppsV1().Deployments(fixtureNamespace).
		Create(ctx, deployment, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating deployment %s: %w", name, err)
	}
	return nil
}

func (c *cluster) podFor(ctx context.Context, workload string) (string, error) {
	pods, err := c.client.CoreV1().Pods(fixtureNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + workload,
		Limit:         5,
	})
	if err != nil {
		return "", fmt.Errorf("listing pods for %s: %w", workload, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod for %s yet", workload)
	}
	return pods.Items[0].Name, nil
}

func (c *cluster) runningPodFor(ctx context.Context, workload string) (string, error) {
	pods, err := c.client.CoreV1().Pods(fixtureNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + workload,
		Limit:         5,
	})
	if err != nil {
		return "", fmt.Errorf("listing pods for %s: %w", workload, err)
	}
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.State.Running != nil {
				return pod.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no running pod for %s yet", workload)
}

func (c *cluster) awaitRestarted(ctx context.Context, workload string) (string, error) {
	for {
		pods, err := c.client.CoreV1().Pods(fixtureNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + workload,
			Limit:         5,
		})
		if err == nil {
			for _, pod := range pods.Items {
				for _, status := range pod.Status.ContainerStatuses {
					if status.RestartCount >= 1 && status.LastTerminationState.Terminated != nil {
						return pod.Name, nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s never restarted: %w", workload, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *cluster) awaitSettled(ctx context.Context) error {
	deployments := c.client.AppsV1().Deployments(fixtureNamespace)
	for {
		deployment, err := deployments.Get(ctx, fixtureWorkload, metav1.GetOptions{})
		if err == nil && deployment.Status.ReadyReplicas >= 1 &&
			deployment.Status.ObservedGeneration >= deployment.Generation {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the fixture workload never became ready: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func ptr[T any](value T) *T { return &value }
