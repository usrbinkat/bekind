package utils

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	goyaml "gopkg.in/yaml.v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/cluster"
)

var decUnstructured = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

// GetDefault selected the default runtime from the environment override
func GetDefaultRuntime() cluster.ProviderOption {
	switch p := os.Getenv("KIND_EXPERIMENTAL_PROVIDER"); p {
	case "":
		return nil
	case "podman":
		log.Warn("using podman due to KIND_EXPERIMENTAL_PROVIDER")
		return cluster.ProviderWithPodman()
	case "docker":
		log.Warn("using docker due to KIND_EXPERIMENTAL_PROVIDER")
		return cluster.ProviderWithDocker()
	default:
		log.Warnf("ignoring unknown value %q for KIND_EXPERIMENTAL_PROVIDER", p)
		return nil
	}
}

// DoSSA  does service side apply with the given YAML as a []byte
func DoSSA(ctx context.Context, cfg *rest.Config, yaml []byte) error {
	// Read yaml into a slice of byte
	yml := yaml

	// get the RESTMapper for the GVR
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	// create dymanic client
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	// read YAML manifest into unstructured.Unstructured
	obj := &unstructured.Unstructured{}
	_, gvk, err := decUnstructured.Decode(yml, nil, obj)
	if err != nil {
		return err
	}

	// Get the GVR
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	// Get the REST interface for the GVR
	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		// namespaced resources should specify the namespace
		dr = dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
	} else {
		// for cluster-wide resources
		dr = dyn.Resource(mapping.Resource)
	}

	// Create object into JSON
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	// Create or Update the obj with service side apply
	//     types.ApplyPatchType indicates service side apply
	//     FieldManager specifies the field owner ID.
	_, err = dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, v1.PatchOptions{
		FieldManager: "fauxpenshift",
	})

	return err
}

//check to see if the named deployment is running
func IsDeploymentRunning(c kubernetes.Interface, ns string, depl string) wait.ConditionFunc {

	return func() (bool, error) {

		// Get the named deployment
		dep, err := c.AppsV1().Deployments(ns).Get(context.TODO(), depl, v1.GetOptions{})

		// If the deployment is not found, that's okay. It means it's not up and running yet
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		// if another error was found, return that
		if err != nil {
			return false, err
		}

		// If the deployment hasn't finsihed, then let's run again
		if dep.Status.ReadyReplicas == 0 {
			return false, nil
		}

		return true, nil

	}
}

// Poll up to timeout seconds for pod to enter running state.
func WaitForDeployment(c kubernetes.Interface, namespace string, deployment string, timeout time.Duration) error {
	return wait.PollImmediate(5*time.Second, timeout, IsDeploymentRunning(c, namespace, deployment))
}

// NewClient returns a kubernetes.Interface
func NewClient(kubeConfigPath string) (kubernetes.Interface, error) {
	if kubeConfigPath == "" {
		kubeConfigPath = os.Getenv("KUBECONFIG")
	}
	if kubeConfigPath == "" {
		kubeConfigPath = clientcmd.RecommendedHomeFile // use default path(.kube/config)
	}
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(kubeConfig)
}

// DownloadFileString will load the contents of a url to a string and return it
func DownloadFileString(url string) (string, error) {
	// Get the data
	r, err := http.Get(url)
	if err != nil {
		return "", err
	}
	//Create a new buffer
	buf := new(strings.Builder)

	// Write the body to file
	_, err = io.Copy(buf, r.Body)
	return buf.String(), err
}

// SplitYAML splits a multipart YAML and returns a slice of a slice of byte
func SplitYAML(resources []byte) ([][]byte, error) {

	dec := goyaml.NewDecoder(bytes.NewReader(resources))

	var res [][]byte
	for {
		var value interface{}
		err := dec.Decode(&value)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		valueBytes, err := goyaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		res = append(res, valueBytes)
	}
	return res, nil
}

// LabelWorkers will label the workers nodes as such
func LabelWorkers(c kubernetes.Interface) error {
	// First select the non control-plane nodes
	workers, err := c.CoreV1().Nodes().List(context.TODO(), v1.ListOptions{
		LabelSelector: `!node-role.kubernetes.io/control-plane`,
	})
	if err != nil {
		return err
	}

	// Loop through and label these nodes as workers
	for _, w := range workers.Items {
		// set up the key and value for the worker
		labelKey := "node-role.kubernetes.io/worker"
		labelValue := ""

		// Apply the labels on the Node object
		labels := w.Labels
		labels[labelKey] = labelValue
		w.SetLabels(labels)

		// Tell the API to update the node
		_, err = c.CoreV1().Nodes().Update(context.TODO(), &w, v1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	// If we made it this far, then we're good
	return nil
}
