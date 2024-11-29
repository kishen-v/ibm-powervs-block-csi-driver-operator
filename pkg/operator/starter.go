package operator

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	opv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformers "github.com/openshift/client-go/config/informers/externalversions"
	v1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/ibm-powervs-block-csi-driver-operator/assets"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csicontrollerset"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	// Operand and operator run in the same namespace
	defaultNamespace      = "openshift-cluster-csi-drivers"
	operatorName          = "ibm-powervs-block-csi-driver-operator"
	operandName           = "ibm-powervs-block-csi-driver"
	cloudCredSecretName   = "ibm-powervs-block-cloud-credentials"
	metricsCertSecretName = "ibm-powervs-block-csi-driver-controller-metrics-serving-cert"
	trustedCAConfigMap    = "ibm-powervs-block-csi-driver-trusted-ca-bundle"
	infrastructureName    = "cluster"
)

var (
	endPointKeyToEnvNameMap = map[string]string{
		"iam": "IBMCLOUD_IAM_API_ENDPOINT",
		"rc":  "IBMCLOUD_RESOURCE_CONTROLLER_API_ENDPOINT",
		"pi":  "IBMCLOUD_POWER_API_ENDPOINT",
	}
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset and informers
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, defaultNamespace, "")
	secretInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().Secrets()
	configMapInformer := kubeInformersForNamespaces.InformersFor(defaultNamespace).Core().V1().ConfigMaps()
	nodeInformer := kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes()

	// Create config clientset and informer. This is used to get the cluster ID
	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	configInformers := configinformers.NewSharedInformerFactory(configClient, 20*time.Minute)
	infraInformer := configInformers.Config().V1().Infrastructures()

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := opv1.SchemeGroupVersion.WithResource("clustercsidrivers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, string(opv1.IBMPowerVSBlockCSIDriver))
	if err != nil {
		return err
	}

	dynamicClient, err := dynamic.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	csiControllerSet := csicontrollerset.NewCSIControllerSet(
		operatorClient,
		controllerConfig.EventRecorder,
	).WithLogLevelController().WithManagementStateController(
		operandName,
		false,
	).WithStaticResourcesController(
		"PowerVSBlockCSIDriverStaticResourcesController",
		kubeClient,
		dynamicClient,
		kubeInformersForNamespaces,
		assets.ReadFile,
		[]string{
			"storageclass_tier1.yaml",
			"storageclass_tier3.yaml",
			"csidriver.yaml",
			"controller_sa.yaml",
			"controller_pdb.yaml",
			"node_sa.yaml",
			"service.yaml",
			"cabundle_cm.yaml",
			"rbac/main_attacher_binding.yaml",
			"rbac/privileged_role.yaml",
			"rbac/controller_privileged_binding.yaml",
			"rbac/node_privileged_binding.yaml",
			"rbac/main_provisioner_binding.yaml",
			"rbac/volumesnapshot_reader_provisioner_binding.yaml",
			"rbac/main_resizer_binding.yaml",
			"rbac/storageclass_reader_resizer_binding.yaml",
			"rbac/csi_node_role.yaml",
			"rbac/csi_node_binding.yaml",
			"rbac/kube_rbac_proxy_role.yaml",
			"rbac/kube_rbac_proxy_binding.yaml",
			"rbac/prometheus_role.yaml",
			"rbac/prometheus_rolebinding.yaml",
			"rbac/lease_leader_election_role.yaml",
			"rbac/lease_leader_election_rolebinding.yaml",
		},
	).WithCSIConfigObserverController(
		"PowerVSBlockDriverCSIConfigObserverController",
		configInformers,
	).WithCSIDriverControllerService(
		"PowerVSBlockDriverControllerServiceController",
		assets.ReadFile,
		"controller.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		configInformers,
		[]factory.Informer{
			nodeInformer.Informer(),
			infraInformer.Informer(),
			secretInformer.Informer(),
			configMapInformer.Informer(),
		},
		csidrivercontrollerservicecontroller.WithObservedProxyDeploymentHook(),
		csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
			defaultNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			cloudCredSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
			defaultNamespace,
			metricsCertSecretName,
			secretInformer,
		),
		csidrivercontrollerservicecontroller.WithReplicasHook(nodeInformer.Lister()),
		withCustomEndPoint(infraInformer.Lister()),
	).WithCSIDriverNodeService(
		"PowerVSBlockDriverNodeServiceController",
		assets.ReadFile,
		"node.yaml",
		kubeClient,
		kubeInformersForNamespaces.InformersFor(defaultNamespace),
		[]factory.Informer{configMapInformer.Informer()},
		csidrivernodeservicecontroller.WithObservedProxyDaemonSetHook(),
		csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
			defaultNamespace,
			trustedCAConfigMap,
			configMapInformer,
		),
	).WithServiceMonitorController(
		"PowerVSBlockCSIServiceMonitorController",
		dynamicClient,
		assets.ReadFile,
		"servicemonitor.yaml",
	)
	klog.Info("Starting the informers")
	go kubeInformersForNamespaces.Start(ctx.Done())
	go dynamicInformers.Start(ctx.Done())
	go configInformers.Start(ctx.Done())

	klog.Info("Starting controllerset")
	go csiControllerSet.Run(ctx, 1)

	<-ctx.Done()

	return nil
}

func withCustomEndPoint(infraLister v1.InfrastructureLister) dc.DeploymentHookFunc {
	return func(_ *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		infra, err := infraLister.Get(infrastructureName)
		if err != nil {
			return err
		}
		if infra.Status.PlatformStatus == nil || infra.Status.PlatformStatus.PowerVS == nil {
			return nil
		}
		serviceEndPoints := infra.Status.PlatformStatus.PowerVS.ServiceEndpoints
		if len(serviceEndPoints) == 0 {
			return nil
		}
		var containerEnvVars []corev1.EnvVar
		for _, serviceEndPoint := range serviceEndPoints {
			containerEnvVars = append(containerEnvVars, corev1.EnvVar{
				Name:  mappedOrCustomEndpoint(serviceEndPoint.Name),
				Value: serviceEndPoint.URL,
			})
		}

		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			if container.Name != "csi-driver" {
				continue
			}
			container.Env = append(container.Env, containerEnvVars...)
			return nil
		}
		return nil
	}
}

// mappedOrCustomEndpoint retrieves the corresponding value for shorthand endpoints
// mentioned under 'serviceEndpoints', else returns the value passed as-is

func mappedOrCustomEndpoint(endpoint string) string {
	// Return the mapped environment keys.
	if endPointKeyToEnvNameMap[endpoint] != "" {
		return endPointKeyToEnvNameMap[endpoint]
	}
	// Return the endpoint as-is
	return endpoint
}
