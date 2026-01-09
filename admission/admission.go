package admission

import (
	"bytes"
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"strconv"
	"text/template"
)

// +kubebuilder:webhook:path=/recut/v1/pod,mutating=true,failurePolicy=Ignore,groups="",resources=pods,verbs=create;update,versions=v1,name=recut-webhook.geek7.io,sideEffects=None,admissionReviewVersions=v1

type PodResourceSaver struct {
	Client  client.Client
	decoder *admission.Decoder
}

type QueryParams struct {
	Namespace string
	PodRegexp string
	Period    string
}

type PodResources map[string]*v1.ResourceRequirements

type PromAnswer struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

const (
	CPU_MIN                        = 100
	ISTIO_PROXY_CPU_MIN            = 5
	DEFAULT_ANNOTATION_DOMAIN      = `recut.geek7.io`
	DEFAULT_MEMORY_LIMIT_MIN       = 1024
	DEFAULT_ISTIO_MEMORY_LIMIT_MIN = 256
	DEFAULT_MEMORY_REQUEST_MIN     = 256
	DEFAULT_PROM_URL               = "http://vmsingle-vm.monitoring:8429"
	DEFAULT_PROM_CPU_QUERY         = "median by (container) (rate(container_cpu_usage_seconds_total{namespace=\"{{ .Namespace }}\", pod=~\"{{ .PodRegexp }}\", image!=\"\", container!=\"POD\"}))[{{ .Period }}]"
	DEFAULT_PROM_MEM_QUERY         = "max(max_over_time(container_memory_working_set_bytes{namespace=\"{{ .Namespace }}\", pod=~\"{{ .PodRegexp }}\", image!=\"\", container!=\"POD\"}[{{ .Period }}])) by (container)"
	DEFAULT_PERIOD                 = "168h"
)

/*

median by (container) (rate(container_cpu_usage_seconds_total{namespace="namespace-stable", pod=~"some-service-[0-9a-z]+-[0-9a-z]+", image!="", container!="POD"}))[120h]
max(max_over_time(container_memory_working_set_bytes{namespace="namespacew-stable", pod=~"some-service-[0-9a-z]+-[0-9a-z]+", image!="", container!="POD"}[120h])) by (container)

*/

var (
	admLog          = ctrl.Log.WithName("recut")
	MIN_CPU_REQUEST = func() int {
		if minCpu := os.Getenv("CPU_REQUEST_MIN"); minCpu != "" {
			if v, err := strconv.Atoi(minCpu); err != nil {
				admLog.Error(err, "Env var CPU_REQUEST_MIN has wrong value. using predefined min")
				return CPU_MIN
			} else {
				return v
			}
		}
		return CPU_MIN
	}
	MIN_ISTIO_PROXY_CPU_REQUEST = func() int {
		if minCpu := os.Getenv("ISTIO_PROXY_CPU_REQUEST_MIN"); minCpu != "" {
			if v, err := strconv.Atoi(minCpu); err != nil {
				admLog.Error(err, "Env var ISTIO_PROXY_CPU_REQUEST_MIN has wrong value. using predefined min")
				return ISTIO_PROXY_CPU_MIN
			} else {
				return v
			}
		}
		return ISTIO_PROXY_CPU_MIN
	}
	ANNOTATION_DOMAIN = func() string {
		if domain := os.Getenv("ANNOTATION_DOMAIN"); domain != "" {
			return domain
		}
		return DEFAULT_ANNOTATION_DOMAIN
	}
	MEMORY_LIMIT_MIN = func() int64 {
		if minMem := os.Getenv("MEMORY_LIMIT_MIN"); minMem != "" {
			if v, err := strconv.ParseInt(minMem, 10, 64); err != nil {
				admLog.Error(err, "Env var MEMORY_LIMIT_MIN has wrong value. using predefined min")
				return DEFAULT_MEMORY_LIMIT_MIN
			} else {
				return v
			}
		}
		return DEFAULT_MEMORY_LIMIT_MIN
	}
	ISTIO_MEMORY_LIMIT_MIN = func() int64 {
		if minMem := os.Getenv("ISTIO_MEMORY_LIMIT_MIN"); minMem != "" {
			if v, err := strconv.ParseInt(minMem, 10, 64); err != nil {
				admLog.Error(err, "Env var MEMORY_LIMIT_MIN has wrong value. using predefined min")
				return DEFAULT_ISTIO_MEMORY_LIMIT_MIN
			} else {
				return v
			}
		}
		return DEFAULT_ISTIO_MEMORY_LIMIT_MIN
	}
	MEMORY_REQUEST_MIN = func() int64 {
		if minMem := os.Getenv("MEMORY_REQUEST_MIN"); minMem != "" {
			if v, err := strconv.ParseInt(minMem, 10, 64); err != nil {
				admLog.Error(err, "Env var MEMORY_REQUEST_MIN has wrong value. using predefined min")
				return DEFAULT_MEMORY_REQUEST_MIN
			} else {
				return v
			}
		}
		return DEFAULT_MEMORY_REQUEST_MIN
	}
	PROM_URL = func() string {
		if promUrl := os.Getenv("PROM_URL"); promUrl == "" {
			admLog.Info("Env var PROM_URL has wrong value or absent. Using predefined value")
			return DEFAULT_PROM_URL
		} else {
			return promUrl
		}
	}
	PROM_CPU_QUERY = func() string {
		if promCpuQuery := os.Getenv("PROM_CPU_QUERY"); promCpuQuery == "" {
			admLog.Info("Env var PROM_CPU_QUERY has wrong value or absent. Using predefined value")
			return DEFAULT_PROM_CPU_QUERY
		} else {
			return promCpuQuery
		}
	}
	PROM_MEM_QUERY = func() string {
		if promMemQuery := os.Getenv("PROM_MEM_QUERY"); promMemQuery == "" {
			admLog.Info("Env var PROM_MEM_QUERY has wrong value or absent. Using predefined value")
			return DEFAULT_PROM_MEM_QUERY
		} else {
			return promMemQuery
		}
	}
	PERIOD = func() string {
		if period := os.Getenv("PERIOD"); period == "" {
			admLog.Info("Env var PERIOD has wrong value or absent. Using predefined value")
			return DEFAULT_PERIOD
		} else {
			return period
		}
	}
	cpuChange = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pod_cpu_request_change",
			Help: "Pod CPU change",
		},
		[]string{
			"namespace",
			"parent",
			"container",
			"advisory",
		},
	)
	memChange = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pod_mem_request_change",
			Help: "Pod MEM change",
		},
		[]string{
			"namespace",
			"parent",
			"container",
			"advisory",
		},
	)
)

func (v *PodResourceSaver) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &v1.Pod{}

	//err := v.decoder.Decode(req, pod)
	err := json.Unmarshal(req.Object.Raw, pod)
	if os.Getenv("LOG_POD_OBJECT") == "yes" {
		bbb, _ := json.Marshal(req.Object.Raw)
		admLog.Info(string(bbb))
	}
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	//admLog.Info(fmt.Sprintf("%v", pod))
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	var (
		promUrl    string
		promQcpu   string
		promQmem   string
		pregexp    string
		parentName string
	)
	originalResources := make(PodResources)

	if len(pod.OwnerReferences) == 0 {
		return admission.Allowed("Won't affect orphan pod")
	}

	if pod.OwnerReferences[0].Kind == "ReplicaSet" {
		parentName = regexp.MustCompile(`^([0-9a-z\-]+)-([0-9a-z]+)-$`).
			ReplaceAllString(pod.GenerateName, "$1")
	}
	if pod.OwnerReferences[0].Kind == "StatefulSet" {
		parentName = regexp.MustCompile(`^([0-9a-z\-]+)-([0-9]+)$`).
			ReplaceAllString(pod.Name, "$1")
	}

	_ = metrics.Registry.Register(cpuChange)
	_ = metrics.Registry.Register(memChange)

	if pod.OwnerReferences == nil {
		admLog.Info("No owner reference. Skipping.", "Namespace", req.Namespace, "Pod", pod.GenerateName)
		return admission.Allowed("No owner reference. Skipping.")
	}
	admLog.Info("Checking annotations ...", "Namespace", req.Namespace, "Pod", pod.GenerateName)
	if pod.Annotations[ANNOTATION_DOMAIN()+"/prom-url"] != "" {
		promUrl = pod.Annotations[ANNOTATION_DOMAIN()+"/prom-url"]
		admLog.Info("prom-url annotaion")

	} else {
		admLog.Info("No "+ANNOTATION_DOMAIN()+"/prom-url annotation. Will use default", pod.OwnerReferences[0].Kind, pod.OwnerReferences[0].Name)
		promUrl = PROM_URL()
	}

	if pod.Annotations[ANNOTATION_DOMAIN()+"/prom-query"] != "" {
		promQcpu = pod.Annotations[ANNOTATION_DOMAIN()+"/prom-query"]
		admLog.Info("prom-query annotation")
	} else {
		admLog.Info("No "+ANNOTATION_DOMAIN()+"/prom-query annotation.", pod.OwnerReferences[0].Kind, pod.OwnerReferences[0].Name)
	}

	if pod.Annotations[ANNOTATION_DOMAIN()+"/mem-prom-query"] != "" {
		promQmem = pod.Annotations[ANNOTATION_DOMAIN()+"/mem-prom-query"]
		admLog.Info("mem-prom-query annotation")
	} else {
		admLog.Info("No "+ANNOTATION_DOMAIN()+"/mem-prom-query annotation.", pod.OwnerReferences[0].Kind, pod.OwnerReferences[0].Name)
	}

	if (promQmem == "") && (promQcpu == "") {
		if def, _ := v.nsIncluded(req.Namespace, ctx); def == "false" {
			return admission.Allowed("No " + ANNOTATION_DOMAIN() + "/prom-query and " + ANNOTATION_DOMAIN() + "/mem-prom-query annotations found. Namespace label absent.")
		}
		if pod.OwnerReferences[0].Kind == "Job" {
			return admission.Allowed("No " + ANNOTATION_DOMAIN() + "/prom-query and " + ANNOTATION_DOMAIN() + "/mem-prom-query annotations found. Namespace label absent.")
		}

		admLog.Info("No " + ANNOTATION_DOMAIN() + "/prom-query and " + ANNOTATION_DOMAIN() + "/mem-prom-query annotations found. Namespace label present, let's go with defaults.")
		if pod.OwnerReferences[0].Kind == "ReplicaSet" {
			pregexp = "^" + regexp.MustCompile(`^([0-9a-z\-]+-)([0-9a-z]+)-$`).
				ReplaceAllString(pod.GenerateName, "$1") + "[0-9a-z]+-[0-9a-z]+$"
		}
		if pod.OwnerReferences[0].Kind == "StatefulSet" {
			pregexp = "^" + regexp.MustCompile(`^([0-9a-z\-]+-)([0-9]+)$`).
				ReplaceAllString(pod.Name, "$1") + "[0-9]+$"
		}
		qp := QueryParams{
			Namespace: req.Namespace,
			PodRegexp: pregexp,
			Period:    PERIOD(),
		}
		buf := &bytes.Buffer{}
		cpuQtmpl, err := template.New("cpuQ").Parse(PROM_CPU_QUERY())
		if err != nil {
			return admission.Allowed("Badly formed template for prometheus CPU Query")
		}
		err = cpuQtmpl.Execute(buf, qp)
		if err != nil {
			return admission.Allowed("Badly formed parameters for prometheus CPU Query")
		}
		promQcpu = buf.String()
		buf1 := &bytes.Buffer{}
		memQtmpl, err := template.New("memQ").Parse(PROM_MEM_QUERY())
		if err != nil {
			return admission.Allowed("Badly formed template for prometheus MEM Query")
		}
		err = memQtmpl.Execute(buf1, qp)
		if err != nil {
			return admission.Allowed("Badly formed parameters for prometheus MEM Query")
		}
		promQmem = buf1.String()
	}

	admLog.Info("Requesting: ", "prom-url", promUrl, "prom-query", promQcpu)

	pr := http.Client{}
	if promQcpu != "" {
		promdata, err := pr.Get(fmt.Sprintf("%s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQcpu)))
		if err != nil {
			admLog.Error(err, "Can't access prometheus")
			return admission.Allowed("Can't access prom 110")
		}

		defer promdata.Body.Close()
		if promdata.StatusCode == http.StatusOK {
			bodyBytes, err := io.ReadAll(promdata.Body)
			if err != nil {
				return admission.Errored(int32(promdata.StatusCode), err)
			}
			admLog.Info(fmt.Sprintf("promdata: %v", string(bodyBytes)))
			metrics := PromAnswer{}
			json.Unmarshal(bodyBytes, &metrics)
			for _, ins := range metrics.Data.Result {
				for i, container := range pod.Spec.Containers {
					if container.Name == ins.Metric["container"] {
						// update cpu request
						var cpu float64
						switch cpuExpr := ins.Value[1].(type) {
						case float64:
							cpu = cpuExpr
						case string:
							cpu, _ = strconv.ParseFloat(cpuExpr, 64)
						}
						var val int64
						if container.Name != "istio-proxy" {
							val = int64(math.Round(math.Max(cpu*1000, float64(MIN_CPU_REQUEST()))))
						} else {
							val = int64(math.Round(math.Max(cpu*1000, float64(MIN_ISTIO_PROXY_CPU_REQUEST()))))
						}
						admLog.Info(fmt.Sprintf(
							"Setting CPU request for container %s to %v instead of %v (calculated values is %v)",
							container.Name,
							val,
							container.Resources.Requests.Cpu().MilliValue(),
							math.Round(cpu*1000),
						))
						cpuChange.With(prometheus.Labels{
							"namespace": req.Namespace,
							"parent":    parentName,
							"container": container.Name,
							"advisory":  fmt.Sprint(v.isAdvisory(req, ctx)),
						}).Set(float64(container.Resources.Requests.Cpu().MilliValue() - val))

						originalResources[container.Name] = new(v1.ResourceRequirements)
						container.Resources.DeepCopyInto(originalResources[container.Name])

						if (pod.Spec.Containers[i].Resources.Requests != nil) &&
							(pod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] != (resource.Quantity{})) {
							pod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = resource.MustParse(fmt.Sprintf("%vm", val))
						}
					}
				}
			}
			pod.Annotations[ANNOTATION_DOMAIN()+"/mutated"] = "yes"

		} else {
			admLog.Info(fmt.Sprintf("Error getting %s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQcpu)))
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}

	admLog.Info("Requesting: ", "prom-url", promUrl, "prom-query", promQmem)

	if promQmem != "" {
		promdata, err := pr.Get(fmt.Sprintf("%s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQmem)))
		if err != nil {
			return admission.Allowed("Can't access prom 110")
		}

		defer promdata.Body.Close()
		if promdata.StatusCode == http.StatusOK {
			bodyBytes, err := io.ReadAll(promdata.Body)
			if err != nil {
				return admission.Errored(int32(promdata.StatusCode), err)
			}
			admLog.Info(fmt.Sprintf("promdata: %v", string(bodyBytes)))
			metrics := PromAnswer{}
			json.Unmarshal(bodyBytes, &metrics)
			for _, ins := range metrics.Data.Result {
				for i, container := range pod.Spec.Containers {
					if container.Name == ins.Metric["container"] {
						// update mem request
						var mem int64
						switch memExpr := ins.Value[1].(type) {
						case int64:
							mem = memExpr
						case string:
							mem, _ = strconv.ParseInt(memExpr, 10, 64)
						}
						var mReq, mLim int64
						mReq = int64(math.Round(math.Max(float64(mem)/1024/1024, float64(MEMORY_REQUEST_MIN()))))
						if container.Name != "istio-proxy" {
							mLim = int64(math.Round(math.Max(float64(mem)*2/1024/1024+100, float64(MEMORY_LIMIT_MIN()))))
						} else {
							mLim = int64(math.Round(math.Max(float64(mem)*2/1024/1024+100, float64(ISTIO_MEMORY_LIMIT_MIN()))))
						}
						admLog.Info(fmt.Sprintf(
							"Setting MEM request for container %s to %vM instead of %vM (calculated values is %vM)",
							container.Name,
							mReq,
							math.Round(float64(container.Resources.Requests.Memory().Value())/1024/1024),
							math.Round(float64(mem)/1024/1024),
						))
						admLog.Info(fmt.Sprintf(
							"Setting MEM limit for container %s to %vM instead of %vM (calculated values is %vM)",
							container.Name,
							mLim,
							math.Round(float64(container.Resources.Limits.Memory().Value())/1024/1024),
							math.Round(float64(mem)*1.5/1024/1024+100),
						))
						memChange.With(prometheus.Labels{
							"namespace": req.Namespace,
							"parent":    parentName,
							"container": container.Name,
							"advisory":  fmt.Sprint(v.isAdvisory(req, ctx)),
						}).Set(math.Round(float64(container.Resources.Requests.Memory().Value())/1024/1024 - float64(mReq)))

						if _, ok := originalResources[container.Name]; !ok {
							originalResources[container.Name] = new(v1.ResourceRequirements)
							container.Resources.DeepCopyInto(originalResources[container.Name])
						}

						if (pod.Spec.Containers[i].Resources.Requests != nil) &&
							(pod.Spec.Containers[i].Resources.Requests[v1.ResourceMemory] != (resource.Quantity{})) {
							pod.Spec.Containers[i].Resources.Requests[v1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%vM", mReq))
						}
						if (pod.Spec.Containers[i].Resources.Limits != nil) &&
							(pod.Spec.Containers[i].Resources.Limits[v1.ResourceMemory] != (resource.Quantity{})) {
							pod.Spec.Containers[i].Resources.Limits[v1.ResourceMemory] = resource.MustParse(fmt.Sprintf("%vM", mLim))
						}
					}
				}
			}
			pod.Annotations[ANNOTATION_DOMAIN()+"/mutated"] = "yes"
		} else {
			admLog.Info(fmt.Sprintf("Error getting %s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQmem)))
			return admission.Errored(http.StatusInternalServerError, err)
		}
	}
	if tempBuf, err := json.Marshal(originalResources); err == nil {
		pod.Annotations[ANNOTATION_DOMAIN()+"/resources"] = string(tempBuf)
	}

	marshaledPod, err := json.Marshal(pod)
	//fmt.Println(string(marshaledPod))
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if v.isAdvisory(req, ctx) {
		return admission.Allowed("Advisory mode")
	} else {
		return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
	}

	/*
		{
		  "status": "success",
		  "data": {
		    "resultType": "vector",
		    "result": [
		      {
		        "metric": {
		          "container": "istio-proxy"
		        },
		        "value": [
		          1666912599,
		          "0.0045883983354834245"
		        ]
		      },
		      {
		        "metric": {
		          "container": "main-service"
		        },
		        "value": [
		          1666912599,
		          "0.01643707094429"
		        ]
		      }
		    ]
		  }
		}
	*/

}

func (v *PodResourceSaver) InjectDecoder(d *admission.Decoder) error {
	v.decoder = d
	return nil
}

func (v *PodResourceSaver) nsIncluded(nsName string, ctx context.Context) (string, error) {
	ns := &v1.Namespace{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: nsName}, ns); err != nil {
		admLog.Error(err, "Failed to get namespace for the pod")
		return "false", err
	} else {
		if ns.Labels[ANNOTATION_DOMAIN()+"/default"] == "enable" || ns.Labels[ANNOTATION_DOMAIN()+"/default"] == "advisory" {
			return ns.Labels[ANNOTATION_DOMAIN()+"/default"], nil
		}
	}
	return "false", nil
}
func (v *PodResourceSaver) isAdvisory(req admission.Request, ctx context.Context) bool {
	ns := &v1.Namespace{}
	if err := v.Client.Get(ctx, types.NamespacedName{Name: req.Namespace}, ns); err != nil {
		admLog.Error(err, "Failed to get namespace for the pod")
		return false
	} else {
		if ns.Labels[ANNOTATION_DOMAIN()+"/default"] == "advisory" {
			return true
		}
	}
	pod := &v1.Pod{}
	_ = json.Unmarshal(req.Object.Raw, pod)
	if pod.Annotations[ANNOTATION_DOMAIN()+"/mode"] == "advisory" {
		return true
	}

	return false
}
