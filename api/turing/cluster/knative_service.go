package cluster

import (
	"context"
	"fmt"
	"math"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	knservingv1 "knative.dev/serving/pkg/apis/serving/v1"

	routerConfig "github.com/caraml-dev/turing/engines/router/missionctl/config"
)

const (
	AutoscalingClassHPA          string = "hpa.autoscaling.knative.dev"
	AutoscalingClassKPA          string = "kpa.autoscaling.knative.dev"
	KnativeServiceLabelKey       string = "serving.knative.dev/service"
	KnativeUserContainerName     string = "user-container"
	DefaultRequestTimeoutSeconds int64  = 30
)

// Autoscaling class values to be used, according to the metric
var autoscalingMetricClassMap = map[string]string{
	"concurrency": AutoscalingClassKPA,
	"rps":         AutoscalingClassKPA,
	"cpu":         AutoscalingClassHPA,
	"memory":      AutoscalingClassHPA,
}

// KnativeService defines the properties for Knative services
type KnativeService struct {
	*BaseService

	IsClusterLocal bool                  `json:"is_cluster_local"`
	ContainerPort  int32                 `json:"containerPort"`
	Protocol       routerConfig.Protocol `json:"protocol"`

	// Autoscaling properties
	MinReplicas       int    `json:"minReplicas"`
	MaxReplicas       int    `json:"maxReplicas"`
	InitialScale      *int   `json:"initialScale"`
	AutoscalingMetric string `json:"autoscalingMetric"`
	// AutoscalingTarget is expected to be an absolute value for concurrency / rps
	// and a % value (of the requested value) for cpu / memory based autoscaling.
	AutoscalingTarget string `json:"autoscalingTarget"`

	// TopologySpreadConstraints contains a list of topology spread constraint to be applied on the pods of this service
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints"`

	// Resource properties
	QueueProxyResourcePercentage int `json:"queueProxyResourcePercentage"`
}

// Creates a new config object compatible with the knative serving API, from
// the given config
func (cfg *KnativeService) BuildKnativeServiceConfig() (*knservingv1.Service, error) {
	// clone creates a copy of a map object
	clone := func(l map[string]string) map[string]string {
		ll := map[string]string{}
		for k, v := range l {
			ll[k] = v
		}
		return ll
	}

	kserviceLabels := clone(cfg.Labels)
	if cfg.IsClusterLocal {
		// Kservice should only be accessible from within the cluster
		// https://knative.dev/v1.2-docs/serving/services/private-services/
		kserviceLabels["networking.knative.dev/visibility"] = "cluster-local"
	}
	kserviceObjectMeta := cfg.buildSvcObjectMeta(kserviceLabels)

	revisionLabels := clone(cfg.Labels)
	kserviceSpec, err := cfg.buildSvcSpec(revisionLabels)
	if err != nil {
		return nil, err
	}

	svc := &knservingv1.Service{
		ObjectMeta: *kserviceObjectMeta,
		Spec:       *kserviceSpec,
	}
	// Call setDefaults on desired knative service here to avoid diffs generated because knative defaulter webhook
	// is called when creating or updating the knative service. Ref: https://github.com/kserve/kserve/blob/v0.8.0
	// /pkg/controller/v1beta1/inferenceservice/reconcilers/knative/ksvc_reconciler.go#L159
	svc.SetDefaults(context.TODO())
	return svc, nil
}

func (cfg *KnativeService) buildSvcObjectMeta(labels map[string]string) *metav1.ObjectMeta {
	return &metav1.ObjectMeta{
		Name:      cfg.Name,
		Namespace: cfg.Namespace,
		Labels:    labels,
	}
}

func (cfg *KnativeService) buildSvcSpec(
	labels map[string]string,
) (*knservingv1.ServiceSpec, error) {
	// Set max timeout for responding to requests
	timeout := DefaultRequestTimeoutSeconds

	autoscalingTarget, err := cfg.getAutoscalingTarget()
	if err != nil {
		return nil, err
	}

	// Build annotations
	annotations := map[string]string{
		"autoscaling.knative.dev/minScale": strconv.Itoa(cfg.MinReplicas),
		"autoscaling.knative.dev/maxScale": strconv.Itoa(cfg.MaxReplicas),
		"autoscaling.knative.dev/metric":   cfg.AutoscalingMetric,
		"autoscaling.knative.dev/target":   autoscalingTarget,
		"autoscaling.knative.dev/class":    autoscalingMetricClassMap[cfg.AutoscalingMetric],
	}

	if cfg.InitialScale != nil {
		annotations["autoscaling.knative.dev/initial-scale"] = strconv.Itoa(*cfg.InitialScale)
	}

	if cfg.QueueProxyResourcePercentage > 0 {
		annotations["queue.sidecar.serving.knative.dev/resourcePercentage"] = strconv.Itoa(cfg.QueueProxyResourcePercentage)
	}

	// Revision name
	revisionName := getDefaultRevisionName(cfg.Name)

	// Build container spec
	var portName string
	// If protocol is using GRPC, add "h2c" which is required for grpc knative
	if cfg.Protocol == routerConfig.UPI {
		portName = "h2c"
	}
	container := corev1.Container{
		Image: cfg.Image,
		Ports: []corev1.ContainerPort{
			{
				Name:          portName,
				ContainerPort: cfg.ContainerPort,
			},
		},
		Resources:    cfg.buildResourceReqs(),
		VolumeMounts: cfg.VolumeMounts,
		Env:          cfg.Envs,
	}
	// to remove after upgrading knative
	if cfg.LivenessHTTPGetPath != "" {
		container.LivenessProbe = cfg.buildContainerProbe(livenessProbeType, int(cfg.ProbePort))
	}
	if cfg.ReadinessHTTPGetPath != "" {
		container.ReadinessProbe = cfg.buildContainerProbe(readinessProbeType, int(cfg.ProbePort))
	}

	// Build initContainer specs if they exist
	var initContainers []corev1.Container
	if cfg.InitContainers != nil {
		initContainers = cfg.buildInitContainer(cfg.InitContainers)
	}

	// Add Knative app name label to the match expressions of each topology spread constraint to spread out all
	// the pods across the specified topologyKey
	topologySpreadConstraints := cfg.appendPodSpreadingLabelSelectorsToTopologySpreadConstraints(revisionName)

	return &knservingv1.ServiceSpec{
		ConfigurationSpec: knservingv1.ConfigurationSpec{
			Template: knservingv1.RevisionTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        revisionName,
					Labels:      labels,
					Annotations: annotations,
				},
				Spec: knservingv1.RevisionSpec{
					PodSpec: corev1.PodSpec{
						Containers:                []corev1.Container{container},
						Volumes:                   cfg.Volumes,
						InitContainers:            initContainers,
						TopologySpreadConstraints: topologySpreadConstraints,
					},
					TimeoutSeconds: &timeout,
				},
			},
		},
	}, nil
}

func (cfg *KnativeService) getAutoscalingTarget() (string, error) {
	switch cfg.AutoscalingMetric {
	case "cpu", "rps":
		// Parse the autoscaling target to an integer
		rawTarget, err := strconv.ParseFloat(cfg.AutoscalingTarget, 64)
		if err != nil {
			return "", err
		}
		targetValue := fmt.Sprintf("%.0f", rawTarget)
		return targetValue, nil
	case "memory":
		// The value is supplied as a % of requested memory but the Knative API expects the value in Mi.
		targetPercent, err := strconv.ParseFloat(cfg.AutoscalingTarget, 64)
		if err != nil {
			return "", err
		}
		targetResource := ComputeResource(cfg.BaseService.MemoryRequests, targetPercent/100)
		// Divide value by (1024^2) to convert to Mi
		return fmt.Sprintf("%.0f", float64(targetResource.Value())/math.Pow(1024, 2)), nil
	case "concurrency":
		// Parse the autoscaling target to a value up to 2 decimal places because Knative allows it
		rawTarget, err := strconv.ParseFloat(cfg.AutoscalingTarget, 64)
		if err != nil {
			return "", err
		}
		targetValue := fmt.Sprintf("%.2f", rawTarget)
		if targetValue == "0.00" {
			return "", fmt.Errorf("concurrency target %v should be at least 0.01 after rounding to 2 decimal places",
				cfg.AutoscalingTarget)
		}
		return targetValue, nil
	}
	// For all other metrics, we can use the supplied value as is.
	return cfg.AutoscalingTarget, nil
}

// appendPodSpreadingLabelSelectorsToTopologySpreadConstraints adds the given revisionName as a label to the
// match labels of each topology spread constraint to spread out all the pods across the specified topologyKey
func (cfg *KnativeService) appendPodSpreadingLabelSelectorsToTopologySpreadConstraints(
	revisionName string,
) []corev1.TopologySpreadConstraint {
	for i := range cfg.TopologySpreadConstraints {
		if cfg.TopologySpreadConstraints[i].LabelSelector == nil {
			cfg.TopologySpreadConstraints[i].LabelSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": revisionName},
			}
		} else {
			if cfg.TopologySpreadConstraints[i].LabelSelector.MatchLabels == nil {
				cfg.TopologySpreadConstraints[i].LabelSelector.MatchLabels = make(map[string]string)
			}
			cfg.TopologySpreadConstraints[i].LabelSelector.MatchLabels["app"] = revisionName
		}
	}
	return cfg.TopologySpreadConstraints
}

func getDefaultRevisionName(serviceName string) string {
	return fmt.Sprintf("%s-0", serviceName)
}
