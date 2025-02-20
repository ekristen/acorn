package containers

import (
	"context"
	"encoding/json"
	"strings"

	api "github.com/acorn-io/acorn/pkg/apis/api.acorn.io"
	apiv1 "github.com/acorn-io/acorn/pkg/apis/api.acorn.io/v1"
	v1 "github.com/acorn-io/acorn/pkg/apis/internal.acorn.io/v1"
	"github.com/acorn-io/acorn/pkg/labels"
	"github.com/acorn-io/acorn/pkg/namespace"
	"github.com/acorn-io/acorn/pkg/tables"
	"github.com/acorn-io/acorn/pkg/watcher"
	"github.com/acorn-io/baaah/pkg/typed"
	"github.com/rancher/wrangler/pkg/data/convert"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func NewStorage(c client.WithWatch) *Storage {
	return &Storage{
		TableConvertor: tables.ContainerConverter,
		client:         c,
	}
}

type Storage struct {
	rest.TableConvertor

	client client.WithWatch
}

func (s *Storage) NewList() runtime.Object {
	return &apiv1.ContainerReplicaList{}
}

func (s *Storage) NamespaceScoped() bool {
	return true
}

func (s *Storage) New() runtime.Object {
	return &apiv1.ContainerReplica{}
}

func (s *Storage) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	con, err := s.get(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if deleteValidation != nil {
		if err := deleteValidation(ctx, con); err != nil {
			return nil, false, err
		}
	}
	return con, true, s.client.Delete(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      con.Status.PodName,
			Namespace: con.Status.PodNamespace,
		},
	})
}

func (s *Storage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return s.get(ctx, name)
}

func newNotFoundContainer(name string) error {
	return apierrors.NewNotFound(schema.GroupResource{
		Group:    api.Group,
		Resource: "containers",
	}, name)
}

func (s *Storage) get(ctx context.Context, name string) (*apiv1.ContainerReplica, error) {
	var ok bool

	ns, podName, err := namespace.DenormalizeName(ctx, s.client, name)
	if apierrors.IsNotFound(err) {
		podName, _, ok = strings.Cut(podName, ".")
		if !ok {
			return nil, newNotFoundContainer(name)
		}
	} else if err != nil {
		return nil, err
	}

	pod := &corev1.Pod{}
	err = s.client.Get(ctx, client.ObjectKey{
		Name:      podName,
		Namespace: ns,
	}, pod)
	if apierrors.IsNotFound(err) {
		return nil, newNotFoundContainer(name)
	} else if err != nil {
		return nil, err
	}

	for _, container := range podToContainers(pod) {
		if container.Name == name {
			return &container, nil
		}
	}

	return nil, newNotFoundContainer(name)
}

func (s *Storage) Watch(ctx context.Context, options *internalversion.ListOptions) (watch.Interface, error) {
	opts := watcher.ListOptions("", options)
	opts.LabelSelector = namespace.Selector(ctx)
	w, err := s.client.Watch(ctx, &corev1.PodList{}, opts)
	if err != nil {
		return nil, err
	}

	return watcher.Transform(w, func(obj runtime.Object) (result []runtime.Object) {
		for _, con := range podToContainers(obj.(*corev1.Pod)) {
			con := con
			if options.FieldSelector != nil {
				if !options.FieldSelector.Matches(fields.Set{
					"metadata.name":      con.Name,
					"metadata.namespace": con.Namespace,
				}) {
					continue
				}
			}
			result = append(result, &con)
		}

		return
	}), nil
}

func (s *Storage) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	pods := &corev1.PodList{}
	err := s.client.List(ctx, pods, &client.ListOptions{
		LabelSelector: namespace.Selector(ctx),
	})
	if err != nil {
		return nil, err
	}

	result := &apiv1.ContainerReplicaList{
		ListMeta: pods.ListMeta,
	}

	for _, pod := range pods.Items {
		result.Items = append(result.Items, podToContainers(&pod)...)
	}

	return result, nil
}

func podToContainers(pod *corev1.Pod) (result []apiv1.ContainerReplica) {
	containerSpecData := []byte(pod.Annotations[labels.AcornContainerSpec])
	if len(containerSpecData) == 0 {
		return nil
	}

	containerSpec := v1.Container{}
	err := json.Unmarshal(containerSpecData, &containerSpec)
	if err != nil {
		logrus.Errorf("failed to unmarshal container spec for %s/%s: %s",
			pod.Namespace, pod.Name, containerSpecData)
		return nil
	}

	imageMapping := map[string]string{}
	imageMappingData := pod.Annotations[labels.AcornImageMapping]
	if len(imageMappingData) > 0 {
		err := json.Unmarshal([]byte(imageMappingData), &imageMapping)
		if err != nil {
			logrus.Errorf("failed to unmarshal image mapping for %s/%s: %s",
				pod.Namespace, pod.Name, imageMappingData)
		}
	}

	for _, sideCarName := range append([]string{""}, typed.SortedKeys(containerSpec.Sidecars)...) {
		replica := containerSpecToContainerReplicaIgnore(pod, imageMapping, containerSpec, sideCarName)
		if replica == nil {
			return nil
		}
		result = append(result, *replica)
	}

	return result
}

func containerSpecToContainerReplicaIgnore(pod *corev1.Pod, imageMapping map[string]string, containerSpec v1.Container, sidecarName string) *apiv1.ContainerReplica {
	result, err := containerSpecToContainerReplica(pod, imageMapping, containerSpec, sidecarName)
	if err != nil {
		logrus.Errorf("failed to convert container spec for %s/%s (, sidecar: [%s]): %v",
			pod.Namespace, pod.Name, sidecarName, err)
		return nil
	}
	return result
}

func containerSpecToContainerReplica(pod *corev1.Pod, imageMapping map[string]string, containerSpec v1.Container, sidecarName string) (*apiv1.ContainerReplica, error) {
	var (
		uid                 = pod.UID
		containerName       = pod.Labels[labels.AcornContainerName]
		jobName             = pod.Labels[labels.AcornJobName]
		containerStatusName = containerName
		namespace, name     = namespace.NormalizedName(pod.ObjectMeta)
	)

	if containerStatusName == "" {
		containerStatusName = jobName
	}

	if sidecarName != "" {
		containerSpec = containerSpec.Sidecars[sidecarName]
		name += "." + sidecarName
		containerStatusName = sidecarName
		uid = types.UID(string(uid) + "-" + sidecarName)
	} else {
		uid = types.UID(string(uid) + "-c")
	}

	result := &apiv1.ContainerReplica{
		ObjectMeta: pod.ObjectMeta,
	}
	if err := convert.ToObj(containerSpec, &result.Spec); err != nil {
		return nil, err
	}

	friendlyImage, ok := imageMapping[result.Spec.Image]
	if ok {
		result.Spec.Image = friendlyImage
	}

	result.Name = name
	result.Namespace = namespace
	result.OwnerReferences = nil
	result.UID = uid
	result.Spec.AppName = pod.Labels[labels.AcornRootPrefix]
	result.Spec.JobName = jobName
	result.Spec.ContainerName = containerName
	result.Spec.SidecarName = sidecarName
	result.Labels = pod.Labels
	result.Annotations = pod.Annotations

	delete(result.Annotations, labels.AcornContainerSpec)

	containerStatus := pod.Status.ContainerStatuses
	if result.Spec.Init {
		containerStatus = pod.Status.InitContainerStatuses
	}

	for _, status := range containerStatus {
		if status.Name != containerStatusName {
			continue
		}

		result.Status = apiv1.ContainerReplicaStatus{
			State:                status.State,
			LastTerminationState: status.LastTerminationState,
			Ready:                status.Ready,
			RestartCount:         status.RestartCount,
			Image:                status.Image,
			ImageID:              status.ImageID,
			Started:              status.Started,
		}

		if status.State.Running != nil {
			if result.Status.Ready {
				result.Status.Columns.State = "running"
			} else {
				result.Status.Columns.State = "running (not ready)"
			}
		} else if status.State.Waiting != nil {
			result.Status.Columns.State = status.State.Waiting.Reason
			if status.State.Waiting.Message != "" {
				result.Status.Columns.State += ": " + status.State.Waiting.Message
			}
		} else if status.State.Terminated != nil {
			if status.State.Terminated.ExitCode == 0 && jobName != "" {
				// Don't include message here because it will be the termination message which
				// is a secret.  We need a secure implementation that doesn't put the secret in the
				// termination message.
				result.Status.Columns.State = "stopped"
			} else {
				result.Status.Columns.State = "stopped: " + status.State.Terminated.Message
			}
		}

		if result.Spec.JobName != "" {
			result.Status.Columns.App = result.Spec.JobName
		} else {
			result.Status.Columns.App = result.Spec.AppName
		}

		break
	}

	result.Status.PodName = pod.Name
	result.Status.PodNamespace = pod.Namespace
	result.Status.Phase = pod.Status.Phase
	result.Status.PodMessage = pod.Status.Message
	result.Status.PodReason = pod.Status.Reason

	return result, nil
}
