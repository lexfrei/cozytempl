package k8s

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const naPlaceholder = "n/a"

// TenantUsage captures resource consumption for a single tenant namespace.
type TenantUsage struct {
	Namespace      string `json:"namespace"`
	PodCount       int    `json:"podCount"`
	CPURequestsMi  int64  `json:"cpuRequestsMi"`
	MemRequestsMi  int64  `json:"memRequestsMi"`
	CPUUsageMi     int64  `json:"cpuUsageMi"`
	MemUsageMi     int64  `json:"memUsageMi"`
	StorageGi      int64  `json:"storageGi"`
	MetricsEnabled bool   `json:"metricsEnabled"`
}

// UsageService reads resource usage per tenant namespace.
type UsageService struct {
	baseCfg *rest.Config
}

// NewUsageService creates a new usage service.
func NewUsageService(baseCfg *rest.Config) *UsageService {
	return &UsageService{baseCfg: baseCfg}
}

// PodGVR returns the GVR for core pods.
func PodGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
}

// PVCGVR returns the GVR for core persistentvolumeclaims.
func PVCGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "", Version: "v1", Resource: "persistentvolumeclaims"}
}

// PodMetricsGVR returns the GVR for metrics.k8s.io/v1beta1 pods.
func PodMetricsGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
}

// Collect returns usage stats for a single tenant namespace.
func (usv *UsageService) Collect(
	ctx context.Context, username string, groups []string, namespace string,
) (TenantUsage, error) {
	client, err := NewImpersonatingClient(usv.baseCfg, username, groups)
	if err != nil {
		return TenantUsage{Namespace: namespace}, err
	}

	usage := TenantUsage{Namespace: namespace}

	podList, podErr := client.Resource(PodGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if podErr == nil {
		usage.PodCount = len(podList.Items)

		for idx := range podList.Items {
			cpu, mem := sumPodRequests(&podList.Items[idx])
			usage.CPURequestsMi += cpu
			usage.MemRequestsMi += mem
		}
	}

	pvcList, pvcErr := client.Resource(PVCGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if pvcErr == nil {
		for idx := range pvcList.Items {
			usage.StorageGi += pvcStorageGi(&pvcList.Items[idx])
		}
	}

	metricsList, metricsErr := client.Resource(PodMetricsGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if metricsErr == nil {
		usage.MetricsEnabled = true

		for idx := range metricsList.Items {
			cpu, mem := sumPodMetrics(&metricsList.Items[idx])
			usage.CPUUsageMi += cpu
			usage.MemUsageMi += mem
		}
	}

	return usage, nil
}

// CollectAll returns usage for multiple tenant namespaces.
func (usv *UsageService) CollectAll(
	ctx context.Context, username string, groups []string, namespaces []string,
) map[string]TenantUsage {
	result := make(map[string]TenantUsage, len(namespaces))

	for _, namespace := range namespaces {
		usage, err := usv.Collect(ctx, username, groups, namespace)
		if err != nil {
			result[namespace] = TenantUsage{Namespace: namespace}

			continue
		}

		result[namespace] = usage
	}

	return result
}

//nolint:gocritic // unnamedResult conflicts with nonamedreturns linter
func sumPodRequests(pod *unstructured.Unstructured) (int64, int64) {
	containers, found, err := unstructured.NestedSlice(pod.Object, "spec", "containers")
	if err != nil || !found {
		return 0, 0
	}

	var cpu, mem int64

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		requests, _, _ := unstructured.NestedStringMap(container, "resources", "requests")
		if val, ok := requests["cpu"]; ok {
			cpu += parseCPUToMilli(val)
		}

		if val, ok := requests["memory"]; ok {
			mem += parseMemToMi(val)
		}
	}

	return cpu, mem
}

func pvcStorageGi(pvc *unstructured.Unstructured) int64 {
	requests, _, _ := unstructured.NestedStringMap(pvc.Object, "spec", "resources", "requests")

	storage, ok := requests["storage"]
	if !ok {
		return 0
	}

	quantity, err := resource.ParseQuantity(storage)
	if err != nil {
		return 0
	}

	bytes := quantity.Value()

	return bytes / (1024 * 1024 * 1024) //nolint:mnd // Gi
}

//nolint:gocritic // unnamedResult conflicts with nonamedreturns linter
func sumPodMetrics(pod *unstructured.Unstructured) (int64, int64) {
	containers, found, err := unstructured.NestedSlice(pod.Object, "containers")
	if err != nil || !found {
		return 0, 0
	}

	var cpu, mem int64

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		usage, _, _ := unstructured.NestedStringMap(container, "usage")
		if val, ok := usage["cpu"]; ok {
			cpu += parseCPUToMilli(val)
		}

		if val, ok := usage["memory"]; ok {
			mem += parseMemToMi(val)
		}
	}

	return cpu, mem
}

func parseCPUToMilli(val string) int64 {
	quantity, err := resource.ParseQuantity(val)
	if err != nil {
		return 0
	}

	return quantity.MilliValue()
}

func parseMemToMi(val string) int64 {
	quantity, err := resource.ParseQuantity(val)
	if err != nil {
		return 0
	}

	bytes := quantity.Value()

	return bytes / (1024 * 1024) //nolint:mnd // Mi
}

// FormatCPU formats millicores as "123m" or "1.2 cores".
func FormatCPU(milli int64) string {
	if milli == 0 {
		return naPlaceholder
	}

	if milli < 1000 { //nolint:mnd // display threshold
		return fmt.Sprintf("%dm", milli)
	}

	return fmt.Sprintf("%.1f cores", float64(milli)/1000) //nolint:mnd // display
}

// FormatMem formats mebibytes as "512 Mi" or "1.5 Gi".
func FormatMem(mebi int64) string {
	if mebi == 0 {
		return naPlaceholder
	}

	if mebi < 1024 { //nolint:mnd // display threshold
		return fmt.Sprintf("%d Mi", mebi)
	}

	return fmt.Sprintf("%.1f Gi", float64(mebi)/1024) //nolint:mnd // display
}

// FormatStorage formats gibibytes as "10 Gi" or "1.5 Ti".
func FormatStorage(gibi int64) string {
	if gibi == 0 {
		return naPlaceholder
	}

	if gibi < 1024 { //nolint:mnd // display threshold
		return fmt.Sprintf("%d Gi", gibi)
	}

	return fmt.Sprintf("%.1f Ti", float64(gibi)/1024) //nolint:mnd // display
}
