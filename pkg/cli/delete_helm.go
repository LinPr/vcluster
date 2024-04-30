package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/loft-sh/log"
	"github.com/loft-sh/vcluster/pkg/cli/find"
	"github.com/loft-sh/vcluster/pkg/cli/flags"
	"github.com/loft-sh/vcluster/pkg/cli/localkubernetes"
	"github.com/loft-sh/vcluster/pkg/helm"
	"github.com/loft-sh/vcluster/pkg/util/clihelper"
	"github.com/loft-sh/vcluster/pkg/util/helmdownloader"
	"github.com/loft-sh/vcluster/pkg/util/translate"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type DeleteOptions struct {
	Manager string

	Wait                bool
	KeepPVC             bool
	DeleteNamespace     bool
	DeleteConfigMap     bool
	AutoDeleteNamespace bool
	IgnoreNotFound      bool

	Project string
}

type deleteHelm struct {
	*flags.GlobalFlags
	*DeleteOptions

	rawConfig  *clientcmdapi.Config
	restConfig *rest.Config
	kubeClient *kubernetes.Clientset

	log log.Logger
}

func DeleteHelm(ctx context.Context, options *DeleteOptions, globalFlags *flags.GlobalFlags, vClusterName string, log log.Logger) error {
	cmd := deleteHelm{
		GlobalFlags:   globalFlags,
		DeleteOptions: options,
		log:           log,
	}

	// find vcluster
	vCluster, err := find.GetVCluster(ctx, cmd.UseKubeConfig, cmd.Context, vClusterName, cmd.Namespace, cmd.log)
	if err != nil {
		if !cmd.IgnoreNotFound {
			return err
		}

		var errorNotFound *find.VClusterNotFoundError
		if !errors.As(err, &errorNotFound) {
			return err
		}

		return nil
	}

	// prepare client
	err = cmd.prepare(vCluster)
	if err != nil {
		return err
	}

	// test for helm
	helmBinaryPath, err := helmdownloader.GetHelmBinaryPath(ctx, cmd.log)
	if err != nil {
		return err
	}

	output, err := exec.Command(helmBinaryPath, "version", "--client", "--template", "{{.Version}}").Output()
	if err != nil {
		return err
	}

	err = clihelper.CheckHelmVersion(string(output))
	if err != nil {
		return err
	}

	// check if namespace
	if cmd.AutoDeleteNamespace {
		namespace, err := cmd.kubeClient.CoreV1().Namespaces().Get(ctx, cmd.Namespace, metav1.GetOptions{})
		if err != nil {
			cmd.log.Debugf("Error retrieving vcluster namespace: %v", err)
		} else if namespace != nil && namespace.Annotations != nil && namespace.Annotations[CreatedByVClusterAnnotation] == "true" {
			cmd.DeleteNamespace = true
		}
	}

	// we have to delete the chart
	cmd.log.Infof("Delete vcluster %s...", vClusterName)
	err = helm.NewClient(cmd.rawConfig, cmd.log, helmBinaryPath).Delete(vClusterName, cmd.Namespace)
	if err != nil {
		return err
	}
	cmd.log.Donef("Successfully deleted virtual cluster %s in namespace %s", vClusterName, cmd.Namespace)

	// try to delete the pvc
	if !cmd.KeepPVC && !cmd.DeleteNamespace {
		pvcName := fmt.Sprintf("data-%s-0", vClusterName)
		pvcNameForK8sAndEks := fmt.Sprintf("data-%s-etcd-0", vClusterName)

		client, err := kubernetes.NewForConfig(cmd.restConfig)
		if err != nil {
			return err
		}

		err = client.CoreV1().PersistentVolumeClaims(cmd.Namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("delete pvc: %w", err)
			}
		} else {
			cmd.log.Donef("Successfully deleted virtual cluster pvc %s in namespace %s", pvcName, cmd.Namespace)
		}

		// Deleting PVC for K8s and eks distro as well.
		err = client.CoreV1().PersistentVolumeClaims(cmd.Namespace).Delete(ctx, pvcNameForK8sAndEks, metav1.DeleteOptions{})
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("delete pvc: %w", err)
			}
		} else {
			cmd.log.Donef("Successfully deleted virtual cluster pvc %s in namespace %s", pvcName, cmd.Namespace)
		}
	}

	// try to delete the ConfigMap
	if cmd.DeleteConfigMap {
		client, err := kubernetes.NewForConfig(cmd.restConfig)
		if err != nil {
			return err
		}

		// Attempt to delete the ConfigMap
		configMapName := fmt.Sprintf("configmap-%s", vClusterName)
		err = client.CoreV1().ConfigMaps(cmd.Namespace).Delete(ctx, configMapName, metav1.DeleteOptions{})
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("delete configmap: %w", err)
			}
		} else {
			cmd.log.Donef("Successfully deleted ConfigMap %s in namespace %s", configMapName, cmd.Namespace)
		}
	}

	// check if there are any other vclusters in the namespace you are deleting vcluster in.
	vClusters, err := find.ListVClusters(ctx, cmd.UseKubeConfig, cmd.Context, "", cmd.Namespace, cmd.log)
	if err != nil {
		return err
	}
	if len(vClusters) > 0 {
		// set to false if there are any virtual clusters running in the same namespace. The vcluster supposed to be deleted by the command has been deleted by now and hence the check should be greater than 0
		cmd.DeleteNamespace = false
	}

	// try to delete the namespace
	if cmd.DeleteNamespace {
		client, err := kubernetes.NewForConfig(cmd.restConfig)
		if err != nil {
			return err
		}

		// delete namespace
		err = client.CoreV1().Namespaces().Delete(ctx, cmd.Namespace, metav1.DeleteOptions{})
		if err != nil {
			if !kerrors.IsNotFound(err) {
				return fmt.Errorf("delete namespace: %w", err)
			}
		} else {
			cmd.log.Donef("Successfully deleted virtual cluster namespace %s", cmd.Namespace)
		}

		// delete multi namespace mode namespaces
		namespaces, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
			LabelSelector: translate.MarkerLabel + "=" + translate.SafeConcatName(cmd.Namespace, "x", vClusterName),
		})
		if err != nil && !kerrors.IsForbidden(err) {
			return fmt.Errorf("list namespaces: %w", err)
		}

		// delete all namespaces
		if namespaces != nil && len(namespaces.Items) > 0 {
			for _, namespace := range namespaces.Items {
				err = client.CoreV1().Namespaces().Delete(ctx, namespace.Name, metav1.DeleteOptions{})
				if err != nil {
					if !kerrors.IsNotFound(err) {
						return fmt.Errorf("delete namespace: %w", err)
					}
				} else {
					cmd.log.Donef("Successfully deleted virtual cluster namespace %s", namespace.Name)
				}
			}
		}

		// wait for vcluster deletion
		if cmd.Wait {
			cmd.log.Info("Waiting for virtual cluster to be deleted...")
			for {
				_, err = client.CoreV1().Namespaces().Get(ctx, cmd.Namespace, metav1.GetOptions{})
				if err != nil {
					break
				}

				time.Sleep(time.Second)
			}
			cmd.log.Done("Virtual Cluster is deleted")
		}
	}

	return nil
}

func (cmd *deleteHelm) prepare(vCluster *find.VCluster) error {
	// load the raw config
	rawConfig, err := vCluster.ClientFactory.RawConfig()
	if err != nil {
		return fmt.Errorf("there is an error loading your current kube config (%w), please make sure you have access to a kubernetes cluster and the command `kubectl get namespaces` is working", err)
	}
	err = deleteContext(&rawConfig, find.VClusterContextName(vCluster.Name, vCluster.Namespace, vCluster.Context), vCluster.Context)
	if err != nil {
		return fmt.Errorf("delete kube context: %w", err)
	}

	rawConfig.CurrentContext = vCluster.Context
	restConfig, err := vCluster.ClientFactory.ClientConfig()
	if err != nil {
		return err
	}

	err = localkubernetes.CleanupLocal(vCluster.Name, vCluster.Namespace, &rawConfig, cmd.log)
	if err != nil {
		cmd.log.Warnf("error cleaning up: %v", err)
	}

	// construct proxy name
	proxyName := find.VClusterConnectBackgroundProxyName(vCluster.Name, vCluster.Namespace, rawConfig.CurrentContext)
	_ = localkubernetes.CleanupBackgroundProxy(proxyName, cmd.log)

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	cmd.Namespace = vCluster.Namespace
	cmd.rawConfig = &rawConfig
	cmd.restConfig = restConfig
	cmd.kubeClient = kubeClient
	return nil
}

func deleteContext(kubeConfig *clientcmdapi.Config, kubeContext string, otherContext string) error {
	// Get context
	contextRaw, ok := kubeConfig.Contexts[kubeContext]
	if !ok {
		return nil
	}

	// Remove context
	delete(kubeConfig.Contexts, kubeContext)

	removeAuthInfo := true
	removeCluster := true

	// Check if AuthInfo or Cluster is used by any other context
	for name, ctx := range kubeConfig.Contexts {
		if name != kubeContext && ctx.AuthInfo == contextRaw.AuthInfo {
			removeAuthInfo = false
		}

		if name != kubeContext && ctx.Cluster == contextRaw.Cluster {
			removeCluster = false
		}
	}

	// Remove AuthInfo if not used by any other context
	if removeAuthInfo {
		delete(kubeConfig.AuthInfos, contextRaw.AuthInfo)
	}

	// Remove Cluster if not used by any other context
	if removeCluster {
		delete(kubeConfig.Clusters, contextRaw.Cluster)
	}

	if kubeConfig.CurrentContext == kubeContext {
		kubeConfig.CurrentContext = ""

		if otherContext != "" {
			kubeConfig.CurrentContext = otherContext
		} else if len(kubeConfig.Contexts) > 0 {
			for contextName, contextObj := range kubeConfig.Contexts {
				if contextObj != nil {
					kubeConfig.CurrentContext = contextName
					break
				}
			}
		}
	}

	return clientcmd.ModifyConfig(clientcmd.NewDefaultClientConfigLoadingRules(), *kubeConfig, false)
}
