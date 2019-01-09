package fuse

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/api/authentication/v1"

	brokerapi "github.com/integr8ly/managed-service-broker/pkg/broker"
	"github.com/integr8ly/managed-service-broker/pkg/clients/openshift"
	appsv1 "github.com/openshift/client-go/apps/clientset/versioned/typed/apps/v1"
	k8sClient "github.com/operator-framework/operator-sdk/pkg/k8sclient"
	"github.com/operator-framework/operator-sdk/pkg/util/k8sutil"
	"github.com/pkg/errors"
	glog "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type FuseDeployer struct {
	id string
}

func NewDeployer(id string) *FuseDeployer {
	return &FuseDeployer{id: id}
}

func (fd *FuseDeployer) IsForService(serviceID string) bool {
	return serviceID == "fuse-service-id"
}

func (fd *FuseDeployer) GetCatalogEntries() []*brokerapi.Service {
	glog.Infof("Getting fuse catalog entries")
	return getCatalogServicesObj()
}

func (fd *FuseDeployer) GetID() string {
	return fd.id
}

func (fd *FuseDeployer) Deploy(instanceID, brokerNamespace string, contextProfile brokerapi.ContextProfile, parameters map[string]interface{}, userInfo v1.UserInfo, k8sclient kubernetes.Interface, osClientFactory *openshift.ClientFactory) (*brokerapi.CreateServiceInstanceResponse, error) {
	glog.Infof("Deploying fuse from deployer, id: %s", instanceID)

	// Namespace
	ns, err := k8sclient.CoreV1().Namespaces().Create(getNamespaceObj("fuse-" + instanceID))
	if err != nil {
		glog.Errorf("failed to create fuse namespace: %+v", err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, errors.Wrap(err, "failed to create namespace for fuse service")
	}

	namespace := ns.ObjectMeta.Name

	// ServiceAccount
	_, err = k8sclient.CoreV1().ServiceAccounts(namespace).Create(getServiceAccountObj())
	if err != nil {
		glog.Errorf("failed to create fuse service account: %+v", err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, errors.Wrap(err, "failed to create service account for fuse service")
	}

	//Role
	_, err = k8sclient.RbacV1beta1().Roles(namespace).Create(getRoleObj())
	if err != nil {
		glog.Errorf("failed to create fuse role: %+v", err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, errors.Wrap(err, "failed to create role for fuse service")
	}

	// RoleBindings
	err = fd.createRoleBindings(namespace, userInfo, k8sclient, osClientFactory)
	if err != nil {
		glog.Errorln(err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, err
	}

	// ImageStream
	err = fd.createImageStream(namespace, osClientFactory)
	if err != nil {
		glog.Errorf("failed to create fuse image stream: %+v", err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, err
	}

	// DeploymentConfig
	err = fd.createFuseOperator(namespace, osClientFactory)
	if err != nil {
		glog.Errorln(err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, err
	}

	// Fuse custom resource
	dashboardURL, err := fd.createFuseCustomResource(namespace, brokerNamespace, contextProfile.Namespace, k8sclient, userInfo.Username, parameters)
	if err != nil {
		glog.Errorln(err)
		return &brokerapi.CreateServiceInstanceResponse{
			Code: http.StatusInternalServerError,
		}, err
	}

	return &brokerapi.CreateServiceInstanceResponse{
		Code:         http.StatusAccepted,
		DashboardURL: dashboardURL,
	}, nil
}

func (fd *FuseDeployer) RemoveDeploy(serviceInstanceId string, namespace string, k8sclient kubernetes.Interface) error {
	ns := "fuse-" + serviceInstanceId
	err := k8sclient.CoreV1().Namespaces().Delete(ns, &metav1.DeleteOptions{})
	if err != nil && !strings.Contains(err.Error(), "not found") {
		glog.Errorf("failed to delete %s namespace: %+v", ns, err)
		return errors.Wrap(err, fmt.Sprintf("failed to delete namespace %s", ns))
	} else if err != nil && strings.Contains(err.Error(), "not found") {
		glog.Infof("fuse namespace already deleted")
	}
	return nil
}

// LastOperation should only return an error if it was unable to check the status, not if the status is failed, when status is failed, set that in the Response object
func (fd *FuseDeployer) LastOperation(instanceID string, k8sclient kubernetes.Interface, osclient *openshift.ClientFactory, operation string) (*brokerapi.LastOperationResponse, error) {
	glog.Infof("Getting last operation for %s", instanceID)
	namespace := "fuse-" + instanceID
	switch operation {
	case "deploy":
		glog.Infof("[LAST OPERATION:DEPLOY] Doing last operation for fuse: %s", namespace)
		podsToWatch := []string{"syndesis-oauthproxy", "syndesis-server", "syndesis-ui"}
		dcClient, err := osclient.AppsClient()
		if err != nil {
			glog.Errorf("failed to create an openshift deployment config client: %+v", err)
			return &brokerapi.LastOperationResponse{
				State:       brokerapi.StateFailed,
				Description: "Failed to create an openshift deployment config client",
			}, errors.Wrap(err, "failed to create an openshift deployment config client")
		}

		nsObj, err := k8sclient.CoreV1().Namespaces().Get(namespace, metav1.GetOptions{})
		if err != nil {
			glog.Infof("[LAST OPERATION:DEPLOY] Failed to get namespace obj for fuse: %s, returning in progress", namespace)
			return &brokerapi.LastOperationResponse{
				State:       brokerapi.StateFailed,
				Description: "Failed to get namespace " + namespace + " for last operation check",
			}, errors.Wrap(err, "failed to get namespace "+namespace+" for last operation check")
		}

		young := false
		if time.Since(nsObj.ObjectMeta.CreationTimestamp.Time).Seconds() <= 120 {
			young = true
		}

		for _, v := range podsToWatch {
			state, description, err := fd.getPodStatus(v, namespace, dcClient)
			if state != brokerapi.StateSucceeded {
				if young {
					glog.Infof("[LAST OPERATION:DEPLOY] %s namespace is younger that 120 secs, returning in progress", namespace)
					err = nil
					state = brokerapi.StateInProgress
				}
				glog.Infof("[LAST OPERATION:DEPLOY] %s namespace is older that 120 secs, returning actual state", namespace)
				return &brokerapi.LastOperationResponse{
					State:       state,
					Description: description,
				}, err
			}
		}

		glog.Infof("[LAST OPERATION:DEPLOY] fuse %s deployed successfully ", namespace)
		return &brokerapi.LastOperationResponse{
			State:       brokerapi.StateSucceeded,
			Description: "fuse deployed successfully",
		}, nil
	case "remove":
		_, err := k8sclient.CoreV1().Namespaces().Get(namespace, metav1.GetOptions{})
		if err != nil && strings.Contains(err.Error(), "not found") {
			return &brokerapi.LastOperationResponse{
				State:       brokerapi.StateSucceeded,
				Description: "fuse removed successfully",
			}, nil
		} else if err != nil {
			//any error getting namespace outside of "not found"
			return &brokerapi.LastOperationResponse{
				State:       brokerapi.StateInProgress,
				Description: "failed to find namespace",
			}, errors.Wrap(err, "could not find namespace")
		}
		return &brokerapi.LastOperationResponse{
			State:       brokerapi.StateInProgress,
			Description: "fuse removal in progress",
		}, nil
	default:
		return &brokerapi.LastOperationResponse{
			State:       brokerapi.StateFailed,
			Description: "unknown operation: " + operation,
		}, nil
	}
}

func (fd *FuseDeployer) createRoleBindings(namespace string, userInfo v1.UserInfo, k8sclient kubernetes.Interface, osClientFactory *openshift.ClientFactory) error {
	for _, sysRoleBinding := range getSystemRoleBindings(namespace) {
		_, err := k8sclient.RbacV1beta1().RoleBindings(namespace).Create(&sysRoleBinding)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return errors.Wrapf(err, "failed to create rolebinding for %s", &sysRoleBinding.ObjectMeta.Name)
		}
	}

	_, err := k8sclient.RbacV1beta1().RoleBindings(namespace).Create(getInstallRoleBindingObj())
	if err != nil {
		return errors.Wrap(err, "failed to create install role binding for fuse service")
	}

	authClient, err := osClientFactory.AuthClient()
	if err != nil {
		return errors.Wrap(err, "failed to create an openshift authorization client")
	}

	_, err = authClient.RoleBindings(namespace).Create(getViewRoleBindingObj())
	if err != nil {
		return errors.Wrap(err, "failed to create view role binding for fuse service")
	}

	_, err = authClient.RoleBindings(namespace).Create(getEditRoleBindingObj())
	if err != nil {
		return errors.Wrap(err, "failed to create edit role binding for fuse service")
	}

	_, err = authClient.RoleBindings(namespace).Create(getUserViewRoleBindingObj(namespace, userInfo.Username))
	if err != nil {
		return errors.Wrap(err, "failed to create user view role binding for fuse service")
	}

	return nil
}

func (fd *FuseDeployer) createImageStream(namespace string, osClientFactory *openshift.ClientFactory) error {
	imageClient, err := osClientFactory.ImageStreamClient()
	if err != nil {
		return errors.Wrap(err, "failed to create an openshift image stream client")
	}

	for _, imgStream := range getFuseOnlineImageStreamsObj() {
		_, err = imageClient.ImageStreams(namespace).Create(&imgStream)
		if err != nil {
			return errors.Wrap(err, "failed to create "+imgStream.ObjectMeta.Name+" image stream for fuse service")
		}
	}

	return nil
}

func (fd *FuseDeployer) createFuseOperator(namespace string, osClientFactory *openshift.ClientFactory) error {
	dcClient, err := osClientFactory.AppsClient()
	if err != nil {
		return errors.Wrap(err, "failed to create an openshift deployment config client")
	}

	_, err = dcClient.DeploymentConfigs(namespace).Create(getDeploymentConfigObj())
	if err != nil {
		return errors.Wrap(err, "failed to create deployment config for fuse service")
	}

	return nil
}

func (fd *FuseDeployer) createFuseCustomResource(namespace, brokerNamespace, userNamespace string, k8sclient kubernetes.Interface, userID string, parameters map[string]interface{}) (string, error) {
	fuseClient, _, err := k8sClient.GetResourceClient("syndesis.io/v1alpha1", "Syndesis", namespace)
	if err != nil {
		return "", errors.Wrap(err, "failed to create fuse client")
	}

	integrationsLimit := 0
	if parameters["limit"] != nil {
		integrationsLimit = int(parameters["limit"].(float64))
	}

	fuseObj := getFuseObj(namespace, userNamespace, integrationsLimit)
	fuseObj.Annotations["syndesis.io/created-by"] = userID

	fuseDashboardURL := fd.getRouteHostname(namespace)

	fuseObj.Spec.RouteHostName = fuseDashboardURL
	_, err = fuseClient.Create(k8sutil.UnstructuredFromRuntimeObject(fuseObj))
	if err != nil {
		return "", errors.Wrap(err, "failed to create a fuse custom resource")
	}

	return "https://" + fuseDashboardURL, nil
}

// Get route hostname for fuse
func (fd *FuseDeployer) getRouteHostname(namespace string) string {
	routeHostname := namespace
	routeSuffix, exists := os.LookupEnv("ROUTE_SUFFIX")
	if exists {
		routeHostname = routeHostname + "." + routeSuffix
	}
	return routeHostname
}

func (fd *FuseDeployer) getPodStatus(podName, namespace string, dcClient *appsv1.AppsV1Client) (string, string, error) {
	pod, err := dcClient.DeploymentConfigs(namespace).Get(podName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to get status of %s: %+v", podName, err)
		return brokerapi.StateFailed,
			"Failed to get status of " + podName,
			errors.Wrap(err, "failed to get status of "+podName)
	}

	for _, v := range pod.Status.Conditions {
		if v.Type == "Ready" && v.Status == "False" {
			return brokerapi.StateInProgress, v.Message, nil
		}
	}

	return brokerapi.StateSucceeded, "", nil
}
