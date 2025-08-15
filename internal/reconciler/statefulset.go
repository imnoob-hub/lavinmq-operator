package reconciler

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"reflect"
	"slices"

	"github.com/cloudamqp/lavinmq-operator/internal/controller/utils"
	resource_utils "github.com/cloudamqp/lavinmq-operator/internal/reconciler/utils"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
)

type StatefulSetReconciler struct {
	*ResourceReconciler
}

func (reconciler *ResourceReconciler) StatefulSetReconciler() *StatefulSetReconciler {
	return &StatefulSetReconciler{
		ResourceReconciler: reconciler,
	}
}

func (b *StatefulSetReconciler) Reconcile(ctx context.Context) (ctrl.Result, error) {
	statefulset, err := b.newObject(ctx)
	if err != nil {
		b.Logger.Error(err, "Failed creating statefulset")
		return ctrl.Result{}, err
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := b.GetItem(ctx, statefulset); err != nil {
			if apierrors.IsNotFound(err) {
				b.CreateItem(ctx, statefulset)
				return nil
			}

			return err
		}

		if err := b.updateFields(ctx, statefulset); err != nil {
			b.Logger.Error(err, "Failed calculating new statefulset")
			return err
		}

		if err := b.Client.Update(ctx, statefulset); err != nil {
			// Conflict errors are expected during retries and do not indicate a critical issue.
			// Logging them would create unnecessary noise in the logs.
			if !apierrors.IsConflict(err) {
				b.Logger.Error(err, "Failed updating new statefulset")
			}

			return err
		}

		return nil
	})

	return ctrl.Result{}, err
}

func (b *StatefulSetReconciler) newObject(ctx context.Context) (*appsv1.StatefulSet, error) {
	labels := utils.LabelsForLavinMQ(b.Instance)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Instance.Name,
			Namespace: b.Instance.Namespace,
			Labels:    labels,
		},
	}

	b.appendSpec(sts)
	b.appendTlsConfig(sts)
	if err := b.setConfigHashAnnotation(ctx, sts); err != nil {
		return nil, err
	}

	return sts, nil
}

func (b *StatefulSetReconciler) appendSpec(sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	configVolumeName := b.Instance.Name

	sts.Spec = appsv1.StatefulSetSpec{
		Replicas: &b.Instance.Spec.Replicas,
		Selector: &metav1.LabelSelector{
			MatchLabels: sts.Labels,
		},
		ServiceName: b.Instance.Name,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      sts.Labels,
				Annotations: make(map[string]string),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:      "lavinmq",
						Image:     b.Instance.Spec.Image,
						Resources: b.Instance.Spec.Resources,
						Command:   []string{"/usr/bin/lavinmq"},
						Args:      b.cliArgs(),
						Ports:     b.portsFromSpec(),
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "data",
								MountPath: "/var/lib/lavinmq",
							},
							{
								Name:      configVolumeName,
								MountPath: "/etc/lavinmq",
								ReadOnly:  true,
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "POD_NAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
							},
							{
								Name: "POD_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						// Startup probe will be used for startup of the container. Once the startup probe succeeds,
						// the liveness and readiness probes will be used.
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "/usr/bin/lavinmqctl status || /usr/bin/lavinmqctl status | grep -q follower"},
								},
							},
							FailureThreshold: 30,
							PeriodSeconds:    10,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "/usr/bin/lavinmqctl status || /usr/bin/lavinmqctl status | grep -q follower"},
								},
							},
							PeriodSeconds: 10,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "/usr/bin/lavinmqctl status || /usr/bin/lavinmqctl status | grep -q follower"},
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: configVolumeName,
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: configVolumeName},
							},
						},
					},
				},
				Affinity: b.Instance.Spec.Affinity,
			},
		},
		VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "data",
					Namespace: b.Instance.Namespace,
				},
				Spec: b.Instance.Spec.DataVolumeClaimSpec,
			},
		},
	}

	return sts
}
func (b *StatefulSetReconciler) portsFromSpec() []corev1.ContainerPort {
	ports := []corev1.ContainerPort{}
	if b.Instance.Spec.EtcdEndpoints != nil {
		ports = appendContainerPort(ports, 5679, "clustering")
	}

	if b.Instance.Spec.Config.Mgmt.Port > 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Mgmt.Port, "http")
	}

	if b.Instance.Spec.Config.Mgmt.TlsPort != 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Mgmt.TlsPort, "https")
	}

	if b.Instance.Spec.Config.Amqp.Port > 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Amqp.Port, "amqp")
	}

	if b.Instance.Spec.Config.Amqp.TlsPort != 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Amqp.TlsPort, "amqps")
	}

	if b.Instance.Spec.Config.Mqtt.Port > 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Mqtt.Port, "mqtt")
	}

	if b.Instance.Spec.Config.Mqtt.TlsPort != 0 {
		ports = appendContainerPort(ports, b.Instance.Spec.Config.Mqtt.TlsPort, "mqtts")
	}

	return ports
}

func appendContainerPort(containerPorts []corev1.ContainerPort, port int32, name string) []corev1.ContainerPort {
	containerPorts = append(containerPorts, corev1.ContainerPort{
		Name:          name,
		ContainerPort: port,
		Protocol:      corev1.ProtocolTCP,
	})
	return containerPorts
}

func (b *StatefulSetReconciler) cliArgs() []string {
	defaultArgs := []string{
		"--bind=0.0.0.0",
		"--guest-only-loopback=false",
	}

	if b.Instance.Spec.Replicas > 0 {
		// Clustering config is currently spread between CLI here and in the config file.
		clusteringArgs := []string{
			fmt.Sprintf("--clustering-advertised-uri=tcp://$(POD_NAME).%s.$(POD_NAMESPACE).svc.cluster.local:5679", b.Instance.Name),
		}
		defaultArgs = append(defaultArgs, clusteringArgs...)
	}

	return defaultArgs
}

func (b *StatefulSetReconciler) appendTlsConfig(sts *appsv1.StatefulSet) {
	if b.Instance.Spec.TlsSecret == nil {
		return
	}

	sts.Spec.Template.Spec.Containers[0].VolumeMounts = append(
		sts.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{
			Name:      "tls",
			MountPath: "/etc/lavinmq/tls",
			ReadOnly:  true,
		},
	)
	sts.Spec.Template.Spec.Volumes = append(
		sts.Spec.Template.Spec.Volumes,
		corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: b.Instance.Spec.TlsSecret.Name,
				},
			},
		},
	)
}

// Used to check if the configmap has changed and restarts the pods if there are any config changes by setting a annotation.
func (b *StatefulSetReconciler) setConfigHashAnnotation(ctx context.Context, sts *appsv1.StatefulSet) error {
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      b.Instance.Name,
			Namespace: b.Instance.Namespace,
		},
	}

	if err := b.GetItem(ctx, configMap); err != nil {
		b.Logger.Error(err, "Failed to fetch ConfigMap", "name", configMap.Name, "namespace", configMap.Namespace)
		return err
	}

	data, exists := configMap.Data[ConfigFileName]
	if !exists {
		err := fmt.Errorf("ConfigMap is missing required key: %s", ConfigFileName)
		b.Logger.Error(err, "ConfigMap is missing required key", "key", ConfigFileName, "name", configMap.Name, "namespace", configMap.Namespace)
		return err
	}

	hash := md5.Sum([]byte(data))
	if sts.Spec.Template.ObjectMeta.Annotations == nil {
		sts.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}

	sts.Spec.Template.ObjectMeta.Annotations["config-hash"] = hex.EncodeToString(hash[:])

	return nil
}

func (b *StatefulSetReconciler) updateFields(ctx context.Context, sts *appsv1.StatefulSet) error {
	if *sts.Spec.Replicas != int32(b.Instance.Spec.Replicas) {
		b.Logger.Info("Replicas changed", "old", sts.Spec.Replicas, "new", b.Instance.Spec.Replicas)
		// TODO: Add support for scaling.
		sts.Spec.Replicas = &b.Instance.Spec.Replicas
	}

	b.diffTemplate(&sts.Spec.Template.Spec)

	if err := b.setConfigHashAnnotation(ctx, sts); err != nil {
		return err
	}

	return nil
}

func (b *StatefulSetReconciler) diffTemplate(old *corev1.PodSpec) {
	// Pointer the old as that's the object we're mutating
	oldContainer := &old.Containers[0]

	if oldContainer.Image != b.Instance.Spec.Image {
		oldContainer.Image = b.Instance.Spec.Image
	}

	if !resource_utils.EqualResourceRequirements(oldContainer.Resources, b.Instance.Spec.Resources) {
		b.Logger.Info("Container resources changed, updating")
		oldContainer.Resources = b.Instance.Spec.Resources
	}

	cliArgs := b.cliArgs()
	// TODO: Expand this to own methods and granular checks
	if !reflect.DeepEqual(oldContainer.Args, cliArgs) {
		b.Logger.Info("cli args changed, updating")
		oldContainer.Args = cliArgs
	}

	if !reflect.DeepEqual(oldContainer.Ports, b.portsFromSpec()) {
		b.Logger.Info("ports changed, updating")
		oldContainer.Ports = b.portsFromSpec()
	}

	index := slices.IndexFunc(old.Volumes, func(v corev1.Volume) bool {
		return v.Name == "tls"
	})

	if index != -1 {
		secretName := old.Volumes[index].VolumeSource.Secret.SecretName
		// Checks if the secret name is the same as the one in the instance spec
		if b.Instance.Spec.TlsSecret != nil && b.Instance.Spec.TlsSecret.Name != secretName {
			b.Logger.Info("tls secret changed, updating")
			old.Volumes[index] = corev1.Volume{
				Name: "tls",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: b.Instance.Spec.TlsSecret.Name,
					},
				},
			}
		}
	} else if b.Instance.Spec.TlsSecret != nil {
		b.Logger.Info("adding tls secret to volumes")
		old.Volumes = append(old.Volumes, corev1.Volume{
			Name: "tls",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: b.Instance.Spec.TlsSecret.Name,
				},
			},
		})
	}
}

// Name returns the name of the statefulset reconciler
func (b *StatefulSetReconciler) Name() string {
	return "statefulset"
}
