package graph

// Options.go holds the option settings for a single graph request.

import (
	"fmt"
	"github.com/kiali/kiali/kubernetes"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	net_http "net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/common/model"

	"github.com/kiali/kiali/business"
	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
)

// The supported vendors
const (
	VendorCytoscape        string = "cytoscape"
	VendorIstio            string = "istio"
	defaultConfigVendor    string = VendorCytoscape
	defaultTelemetryVendor string = VendorIstio
)

const (
	GroupByApp                string = "app"
	GroupByNone               string = "none"
	GroupByVersion            string = "version"
	NamespaceIstio            string = "istio-system"
	defaultDuration           string = "10m"
	defaultGraphType          string = GraphTypeWorkload
	defaultGroupBy            string = GroupByNone
	defaultInjectServiceNodes bool   = false
)

const (
	graphKindNamespace string = "namespace"
	graphKindNode      string = "node"
)

// NodeOptions are those that apply only to node-detail graphs
type NodeOptions struct {
	App       string
	Namespace string
	Service   string
	Version   string
	Workload  string
}

// CommonOptions are those supplied to Telemetry and Config Vendors
type CommonOptions struct {
	Duration  time.Duration
	GraphType string
	Params    url.Values // make available the raw query params for vendor-specific handling
	QueryTime int64      // unix time in seconds
}

// ConfigOptions are those supplied to Config Vendors
type ConfigOptions struct {
	GroupBy string
	CommonOptions
}

type RequestedAppenders struct {
	All           bool
	AppenderNames []string
}

// TelemetryOptions are those supplied to Telemetry Vendors
type TelemetryOptions struct {
	AccessibleNamespaces map[string]time.Time
	Appenders            RequestedAppenders // requested appenders, nil if param not supplied
	InjectServiceNodes   bool               // inject destination service nodes between source and destination nodes.
	Namespaces           NamespaceInfoMap
	CommonOptions
	NodeOptions
}

// Options comprises all available options
type Options struct {
	Context         string
	ConfigVendor    string
	TelemetryVendor string
	ConfigOptions
	TelemetryOptions
}

func NewOptions(r *net_http.Request) Options {
	// path variables (0 or more will be set)
	vars := mux.Vars(r)
	app := vars["app"]
	namespace := vars["namespace"]
	service := vars["service"]
	version := vars["version"]
	workload := vars["workload"]

	// query params
	params := r.URL.Query()
	var duration model.Duration
	var injectServiceNodes bool
	var queryTime int64
	appenders := RequestedAppenders{All: true}
	configVendor := params.Get("configVendor")
	durationString := params.Get("duration")
	graphType := params.Get("graphType")
	groupBy := params.Get("groupBy")
	injectServiceNodesString := params.Get("injectServiceNodes")
	namespaces := params.Get("namespaces") // csl of namespaces
	queryTimeString := params.Get("queryTime")
	context := params.Get("context")
	telemetryVendor := params.Get("telemetryVendor")

	if _, ok := params["appenders"]; ok {
		appenderNames := strings.Split(params.Get("appenders"), ",")
		for i, appenderName := range appenderNames {
			appenderNames[i] = strings.TrimSpace(appenderName)
		}
		appenders = RequestedAppenders{All: false, AppenderNames: appenderNames}
	}

	if configVendor == "" {
		configVendor = defaultConfigVendor
	} else if configVendor != VendorCytoscape {
		BadRequest(fmt.Sprintf("Invalid configVendor [%s]", configVendor))
	}
	if durationString == "" {
		duration, _ = model.ParseDuration(defaultDuration)
	} else {
		var durationErr error
		duration, durationErr = model.ParseDuration(durationString)
		if durationErr != nil {
			BadRequest(fmt.Sprintf("Invalid duration [%s]", durationString))
		}
	}
	if graphType == "" {
		graphType = defaultGraphType
	} else if graphType != GraphTypeApp && graphType != GraphTypeService && graphType != GraphTypeVersionedApp && graphType != GraphTypeWorkload {
		BadRequest(fmt.Sprintf("Invalid graphType [%s]", graphType))
	}
	// app node graphs require an app graph type
	if app != "" && graphType != GraphTypeApp && graphType != GraphTypeVersionedApp {
		BadRequest(fmt.Sprintf("Invalid graphType [%s]. This node detail graph supports only graphType app or versionedApp.", graphType))
	}
	if groupBy == "" {
		groupBy = defaultGroupBy
	} else if groupBy != GroupByApp && groupBy != GroupByNone && groupBy != GroupByVersion {
		BadRequest(fmt.Sprintf("Invalid groupBy [%s]", groupBy))
	}
	if injectServiceNodesString == "" {
		injectServiceNodes = defaultInjectServiceNodes
	} else {
		var injectServiceNodesErr error
		injectServiceNodes, injectServiceNodesErr = strconv.ParseBool(injectServiceNodesString)
		if injectServiceNodesErr != nil {
			BadRequest(fmt.Sprintf("Invalid injectServiceNodes [%s]", injectServiceNodesString))
		}
	}
	if queryTimeString == "" {
		queryTime = time.Now().Unix()
	} else {
		var queryTimeErr error
		queryTime, queryTimeErr = strconv.ParseInt(queryTimeString, 10, 64)
		if queryTimeErr != nil {
			BadRequest(fmt.Sprintf("Invalid queryTime [%s]", queryTimeString))
		}
	}
	if telemetryVendor == "" {
		telemetryVendor = defaultTelemetryVendor
	} else if telemetryVendor != VendorIstio {
		BadRequest(fmt.Sprintf("Invalid telemetryVendor [%s]", telemetryVendor))
	}

	// Process namespaces options:
	namespaceMap := NewNamespaceInfoMap()

	/*	tokenContext := r.Context().Value("token")
		var token string
		if tokenContext != nil {
			if tokenString, ok := tokenContext.(string); !ok {
				Error("token is not of type string")
			} else {
				token = tokenString
			}
		} else {
			Error("token missing in request context")
		}*/

	accessibleNamespaces := getAccessibleNamespacesNoToken(context)

	// If path variable is set then it is the only relevant namespace (it's a node graph)
	// Else if namespaces query param is set it specifies the relevant namespaces
	// Else error, at least one namespace is required.
	if namespace != "" {
		namespaces = namespace
	}

	if namespaces == "" {
		BadRequest(fmt.Sprintf("At least one namespace must be specified via the namespaces query parameter."))
	}

	for _, namespaceToken := range strings.Split(namespaces, ",") {
		namespaceToken = strings.TrimSpace(namespaceToken)
		if creationTime, found := accessibleNamespaces[namespaceToken]; found {
			namespaceMap[namespaceToken] = NamespaceInfo{
				Name:     namespaceToken,
				Duration: getSafeNamespaceDuration(namespaceToken, creationTime, time.Duration(duration), queryTime),
				IsIstio:  config.IsIstioNamespace(namespaceToken),
			}
		} else {
			Forbidden(fmt.Sprintf("Requested namespace [%s] is not accessible.", namespaceToken))
		}
	}

	// Service graphs require service injection
	if graphType == GraphTypeService {
		injectServiceNodes = true
	}

	options := Options{
		Context:         context,
		ConfigVendor:    configVendor,
		TelemetryVendor: telemetryVendor,
		ConfigOptions: ConfigOptions{
			GroupBy: groupBy,
			CommonOptions: CommonOptions{
				Duration:  time.Duration(duration),
				GraphType: graphType,
				Params:    params,
				QueryTime: queryTime,
			},
		},
		TelemetryOptions: TelemetryOptions{
			AccessibleNamespaces: accessibleNamespaces,
			Appenders:            appenders,
			InjectServiceNodes:   injectServiceNodes,
			Namespaces:           namespaceMap,
			CommonOptions: CommonOptions{
				Duration:  time.Duration(duration),
				GraphType: graphType,
				Params:    params,
				QueryTime: queryTime,
			},
			NodeOptions: NodeOptions{
				App:       app,
				Namespace: namespace,
				Service:   service,
				Version:   version,
				Workload:  workload,
			},
		},
	}

	return options
}

// GetGraphKind will return the kind of graph represented by the options.
func (o *TelemetryOptions) GetGraphKind() string {
	if o.NodeOptions.App != "" ||
		o.NodeOptions.Version != "" ||
		o.NodeOptions.Workload != "" ||
		o.NodeOptions.Service != "" {
		return graphKindNode
	}
	return graphKindNamespace
}

// getAccessibleNamespaces returns a Set of all namespaces accessible to the user.
// The Set is implemented using the map convention. Each map entry is set to the
// creation timestamp of the namespace, to be used to ensure valid time ranges for
// queries against the namespace.
func getAccessibleNamespaces(token string) map[string]time.Time {
	// Get the namespaces
	business, err := business.Get(token)
	CheckError(err)

	namespaces, err := business.Namespace.GetNamespaces()
	CheckError(err)

	// Create a map to store the namespaces
	namespaceMap := make(map[string]time.Time)
	for _, namespace := range namespaces {
		namespaceMap[namespace.Name] = namespace.CreationTimestamp
	}

	return namespaceMap
}

// getAccessibleNamespaces returns a Set of all namespaces accessible to the user.
// The Set is implemented using the map convention. Each map entry is set to the
// creation timestamp of the namespace, to be used to ensure valid time ranges for
// queries against the namespace.
func getAccessibleNamespacesNoToken(context string) map[string]time.Time {
	namespaceMap := make(map[string]time.Time)
	configMap, err := GetConfigMap(context)
	if err != nil {
		return namespaceMap
	}
	clientSet, err := kubernetes.GetK8sClientSet(configMap.BinaryData[KubeConfig])
	if err != nil {
		return namespaceMap
	}
	ops := metav1.ListOptions{}
	ns, err := clientSet.CoreV1().Namespaces().List(ops)
	if err != nil {
		return namespaceMap
	}

	for _, namespace := range ns.Items {
		namespaceMap[namespace.Name] = namespace.CreationTimestamp.Time
	}
	return namespaceMap
}

const (
	DefaultNamespace = "service-mesh"
	KubeConfig       = "kubeConfig"
	App              = "app"
	Mesher           = "mesher"
)

func GetConfigMap(name string) (configMap *v1.ConfigMap, err error) {
	ops := metav1.GetOptions{}
	clientSet, err := kubernetes.GetDefaultK8sClientSet()
	if err != nil {
		return nil, err
	}
	return clientSet.CoreV1().ConfigMaps(DefaultNamespace).Get(name, ops)
}

// getSafeNamespaceDuration returns a safe duration for the query. If queryTime-requestedDuration > namespace
// creation time just return the requestedDuration.  Otherwise reduce the duration as needed to ensure the
// namespace existed for the entire time range.  An error is generated if no safe duration exists (i.e. the
// queryTime precedes the namespace).
func getSafeNamespaceDuration(ns string, nsCreationTime time.Time, requestedDuration time.Duration, queryTime int64) time.Duration {
	var endTime time.Time
	safeDuration := requestedDuration

	if !nsCreationTime.IsZero() {
		if queryTime != 0 {
			endTime = time.Unix(queryTime, 0)
		} else {
			endTime = time.Now()
		}

		nsLifetime := endTime.Sub(nsCreationTime)
		if nsLifetime <= 0 {
			BadRequest(fmt.Sprintf("Namespace [%s] did not exist at requested queryTime [%v]", ns, endTime))
		}

		if nsLifetime < safeDuration {
			safeDuration = nsLifetime
			log.Debugf("Reducing requestedDuration [%v] to safeDuration [%v]", requestedDuration, safeDuration)
		}
	}

	return safeDuration
}
