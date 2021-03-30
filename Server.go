package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gopkg.in/yaml.v2"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
	defaulter     = runtime.ObjectDefaulter(runtimeScheme)
)
var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	metav1.NamespacePublic,
}

const (
	admissionWebhookAnnotationInjectKey = "it"
)

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func main() {
	log.Print("start")
	whsvr := &WebhookServer{}
	mux := http.NewServeMux()
	pair, err := tls.LoadX509KeyPair("key/tls.crt", "key/tls.key")
	if err != nil {
		panic(err.Error())
	}
	mux.HandleFunc("/mutate", whsvr.mutate)
	whsvr.server = &http.Server{
		Addr:      fmt.Sprintf(":%v", os.Getenv("port")),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}},
	}
	whsvr.server.Handler = mux
	go func() {
		if err := whsvr.server.ListenAndServeTLS("", ""); err != nil {
			panic(err.Error())
		}
	}()
	log.Print("running")
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan
	whsvr.server.Shutdown(context.Background())
}

type WebhookServer struct {
	sidecarConfig *Config
	server        *http.Server
}

type Config struct {
	Containers []corev1.Container `json:"containers"`
	Volumes    []corev1.Volume    `json:"volumes"`
}

func (this *WebhookServer) mutate(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Err:%s", err.Error())
		http.Error(w, "invalid Content-Type, expect application/json", http.StatusBadRequest)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Printf("Err:%s", "invalid Content-Type")
		http.Error(w, "invalid Content-Type", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(data, nil, &ar); err != nil {
		fmt.Printf(err.Error())
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = this.inject(&ar)
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}
	resp, err := json.Marshal(admissionReview)
	if err != nil {
		log.Printf("Err:%s", err.Error())
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}

	if _, err := w.Write(resp); err != nil {
		log.Printf("Err:%s", err.Error())
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}

}

func (whsvr *WebhookServer) inject(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Printf("Err:%s", err.Error())
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}
	log.Printf("Info:pod name %s", pod.Name)
	var b bool
	b, whsvr.sidecarConfig = mutationRequired(ignoredNamespaces, &pod.ObjectMeta)
	if !b {
		log.Print("skip mutation")
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	patchBytes, err := createPatch(&pod, whsvr.sidecarConfig)
	if err != nil {
		log.Printf("Err:%s", err.Error())
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}
	log.Printf("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func mutationRequired(ignoredList []string, metadata *metav1.ObjectMeta) (bool, *Config) {
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			return false, nil
		}
	}
	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	c, err := loadConfig(os.Getenv("confpath") + "/" + annotations["file"])
	//c, err := loadConfig(os.Getenv("confpath"))
	if err != nil {
		log.Printf("Err:%s", err.Error())
		return false, nil
	}
	for k, v := range annotations {
		log.Printf("%s:%s\n", k, v)
		annotationsarry := strings.Split(k, ".")
		if annotationsarry[0] == admissionWebhookAnnotationInjectKey {
			for i, cs := range c.Containers {
				if cs.Name == annotationsarry[1] {
					tmpimg := strings.Split(cs.Image, ":")
					cs.Image = tmpimg[0] + ":" + v
				}
				c.Containers[i] = cs
			}
		}
	}
	if len(c.Containers) == 0 {
		return false, nil
	}
	return true, c
}

func createPatch(pod *corev1.Pod, sidecarConfig *Config) ([]byte, error) {
	var patch []patchOperation
	patch = append(patch, addContainer(pod.Spec.Containers, sidecarConfig.Containers, "/spec/containers")...)
	patch = append(patch, addVolume(pod.Spec.Volumes, sidecarConfig.Volumes, "/spec/volumes")...)
	return json.Marshal(patch)
}

func addContainer(target, added []corev1.Container, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}
func addVolume(target, added []corev1.Volume, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}
func loadConfig(configFile string) (*Config, error) {
	var body interface{}
	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	var cfg Config
	log.Printf("%s\n", string(data))
	if err := yaml.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	body = convert(body)
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	json.Unmarshal(b, &cfg)
	return &cfg, nil
}
func convert(i interface{}) interface{} {
	switch x := i.(type) {
	case map[interface{}]interface{}:
		m2 := map[string]interface{}{}
		for k, v := range x {
			m2[k.(string)] = convert(v)
		}
		return m2
	case []interface{}:
		for i, v := range x {
			x[i] = convert(v)
		}
	}
	return i
}
