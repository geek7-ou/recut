package admission

import (
	"context"
	"fmt"
	"io"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/json"
	"math"
	"net/http"
	"net/url"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"strconv"
)

// +kubebuilder:webhook:path=/recut/v1/pod,mutating=true,failurePolicy=Ignore,groups="",resources=pods,verbs=create;update,versions=v1,name=recut-webhook.geek7.io,sideEffects=None,admissionReviewVersions=v1

type PodResourceSaver struct {
	Client  client.Client
	decoder *admission.Decoder
}

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
	CPU_MIN                   = 100
	ISTIO_PROXY_CPU_MIN       = 5
	DEFAULT_ANNOTATION_DOMAIN = `recut.geek7.io`
)

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
				admLog.Error(err, "Env var CPU_REQUEST_MIN has wrong value. using predefined min")
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
)

// PrometheusRuleValidator admits a PrometheusRule if a specific set of Rule labels exist
func (v *PodResourceSaver) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &v1.Pod{}

	err := v.decoder.Decode(req, pod)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	//admLog.Info(fmt.Sprintf("%v", pod))
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	var (
		promUrl string
		promQ   string
	)

	if pod.Annotations[ANNOTATION_DOMAIN()+"/prom-url"] != "" {
		promUrl = pod.Annotations[ANNOTATION_DOMAIN()+"/prom-url"]
	} else {
		admLog.Info("No "+ANNOTATION_DOMAIN()+"/prom-url annotation.", pod.OwnerReferences[0].Kind, pod.OwnerReferences[0].Name)
		return admission.Allowed("No recut.geek7.io/prom-url annotation")
	}

	if pod.Annotations[ANNOTATION_DOMAIN()+"/prom-query"] != "" {
		promQ = pod.Annotations[ANNOTATION_DOMAIN()+"/prom-query"]
	} else {
		admLog.Info("No "+ANNOTATION_DOMAIN()+"/prom-query annotation.", pod.OwnerReferences[0].Kind, pod.OwnerReferences[0].Name)
		return admission.Allowed("No " + ANNOTATION_DOMAIN() + "/prom-query annotation")
	}
	admLog.Info("Annotations: ", "prom-url", promUrl, "prom-query", promQ)
	pr := http.Client{}
	promdata, err := pr.Get(fmt.Sprintf("%s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQ)))
	if err != nil {
		return admission.Errored(int32(promdata.StatusCode), err)
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
					var v int64
					if container.Name != "istio-proxy" {
						v = int64(math.Round(math.Max(cpu*1000, float64(MIN_CPU_REQUEST()))))
					} else {
						v = int64(math.Round(math.Max(cpu*1000, float64(MIN_ISTIO_PROXY_CPU_REQUEST()))))
					}
					admLog.Info(fmt.Sprintf(
						"Setting CPU request for container %s to %v instead of %v (calculated values is %v)",
						container.Name,
						v,
						container.Resources.Requests.Cpu().MilliValue(),
						math.Round(cpu*1000),
					))
					pod.Spec.Containers[i].Resources.Requests[v1.ResourceCPU] = resource.MustParse(fmt.Sprintf("%vm", v))
				}
			}
		}
		pod.Annotations[ANNOTATION_DOMAIN()+"/mutated"] = "yes"

		marshaledPod, err := json.Marshal(pod)
		//fmt.Println(string(marshaledPod))
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}

		return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
	} else {
		admLog.Info(fmt.Sprintf("cant access %s/api/v1/query?query=%s", promUrl, url.QueryEscape(promQ)))
		return admission.Errored(http.StatusInternalServerError, err)
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
